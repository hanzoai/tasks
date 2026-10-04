// Copyright © 2026 Hanzo AI. MIT License.

package store

// sqlkv.go keeps every shard's rows in ONE table on a shared SQL server — hanzoai/sql,
// reached over ZAP — so a process that runs the engine keeps nothing on its disk and
// any process can take its place.
//
//	tasks_kv(principal text, ns text, key bytea, value bytea, upd bigint,
//	         PRIMARY KEY (principal, ns, key))
//
// A shard is (principal, ns); its rows are the ones that name it. The key is bytes,
// compared as bytes, so a scan walks keys in exactly the order a shard's SQLite file
// does, whatever the server's collation — and a prefix's upper bound needs no valid
// UTF-8. Each value is sealed before it leaves the process under a key derived, as a
// shard file's is, from the master and the tenant (cek.DeriveKey): the server holds
// ciphertext it cannot read, and a row lifted into another shard's slot fails its tag.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hanzoai/cek"
	"github.com/hanzoai/namespace"
	"github.com/valyala/fasthttp"
	zaphttp "github.com/zap-proto/http"
)

// Table is the one table every shard's rows share.
const Table = "tasks_kv"

const tableDDL = `CREATE TABLE IF NOT EXISTS ` + Table + ` (
  principal text   NOT NULL,
  ns        text   NOT NULL,
  key       bytea  NOT NULL,
  value     bytea  NOT NULL,
  upd       bigint NOT NULL,
  PRIMARY KEY (principal, ns, key)
)`

// server is the ZAP client for hanzoai/sql: each statement is a POST of
// {sql, args} to /exec or /query, and a query answers its rows as JSON objects.
// Every argument reaches the server as text, so a statement casts what is not.
type server struct {
	t       *zaphttp.Transport
	addr    string
	timeout time.Duration
}

func dialServer(addr string) *server {
	t := zaphttp.Dial("tcp", addr)
	t.SetReadTimeout(30 * time.Second)
	return &server{t: t, addr: addr, timeout: 30 * time.Second}
}

func (s *server) call(ctx context.Context, path, stmt string, args ...any) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"sql": stmt, "args": args})
	if err != nil {
		return nil, err
	}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetRequestURI(path)
	req.Header.SetHost(s.addr)
	req.Header.SetContentType("application/json")
	req.SetBody(body)
	done := make(chan error, 1)
	go func() { done <- s.t.Do(req, resp) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		// The transport cannot be interrupted; its read timeout bounds it. The
		// request is abandoned, and its exchange finishes on its own.
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("store: sql %s: %w", path, err)
	}
	out := append([]byte(nil), resp.Body()...)
	if c := resp.StatusCode(); c != 200 {
		return nil, fmt.Errorf("store: sql %s answered %d: %s", path, c, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (s *server) exec(ctx context.Context, stmt string, args ...any) error {
	_, err := s.call(ctx, "/exec", stmt, args...)
	return err
}

func (s *server) query(ctx context.Context, stmt string, args ...any) ([]map[string]any, error) {
	b, err := s.call(ctx, "/query", stmt, args...)
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("store: sql query answered no rows: %w", err)
	}
	return rows, nil
}

func (s *server) close() { s.t.CloseIdleConnections() }

// col reads one text column of a row.
func col(row map[string]any, name string) (string, error) {
	v, ok := row[name].(string)
	if !ok {
		return "", fmt.Errorf("store: sql row has no text %q: %v", name, row[name])
	}
	return v, nil
}

// sqlKV is one shard's rows in the shared table.
type sqlKV struct {
	srv       *server
	principal string
	ns        string
	aead      cipher.AEAD
	aad       []byte
}

func newSQLKV(srv *server, master []byte, tenant namespace.Namespace, p Principal, ns string) (*sqlKV, error) {
	key, err := cek.DeriveKey(master, tenant, ns)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sqlKV{srv: srv, principal: p.String(), ns: ns, aead: aead, aad: []byte("hanzo/tasks/kv/v1\x00" + p.String() + "\x00" + ns)}, nil
}

// hx is bytes as the server's hex: base64 is what the server would wrap at 76
// characters, and a line break inside a JSON string is not one the reply escapes.
func hx(b []byte) string { return hex.EncodeToString(b) }

// unhx reads a column a query rendered as 'h' || encode(x, 'hex'). The letter is
// there because the listener writes a text value that begins with a digit as a bare
// JSON number, and hex begins with one half the time.
func unhx(s string) ([]byte, error) {
	if !strings.HasPrefix(s, "h") {
		return nil, fmt.Errorf("store: sql column %q is not h-prefixed hex", s)
	}
	return hex.DecodeString(s[1:])
}

// seal is nonce||ciphertext, the AAD binding it to its shard and key.
func (k *sqlKV) seal(key string, value []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize(), k.aead.NonceSize()+len(value)+k.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, value, append(k.aad, "\x00"+key...)), nil
}

func (k *sqlKV) open(key string, sealed []byte) ([]byte, error) {
	n := k.aead.NonceSize()
	if len(sealed) < n {
		return nil, fmt.Errorf("store: sealed value of %q is too short", key)
	}
	return k.aead.Open(nil, sealed[:n], sealed[n:], append(k.aad, "\x00"+key...))
}

func (k *sqlKV) row(r map[string]any) (string, []byte, error) {
	ks, err := col(r, "k")
	if err != nil {
		return "", nil, err
	}
	vs, err := col(r, "v")
	if err != nil {
		return "", nil, err
	}
	kb, err := unhx(ks)
	if err != nil {
		return "", nil, err
	}
	vb, err := unhx(vs)
	if err != nil {
		return "", nil, err
	}
	v, err := k.open(string(kb), vb)
	if err != nil {
		return "", nil, fmt.Errorf("store: open %s/%s %q: %w", k.principal, k.ns, kb, err)
	}
	return string(kb), v, nil
}

func (k *sqlKV) get(ctx context.Context, key string) ([]byte, bool, error) {
	rows, err := k.srv.query(ctx, `SELECT 'h' || encode(key,'hex') AS k, 'h' || encode(value,'hex') AS v FROM `+Table+
		` WHERE principal=$1 AND ns=$2 AND key=decode($3,'hex')`, k.principal, k.ns, hx([]byte(key)))
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	_, v, err := k.row(rows[0])
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func (k *sqlKV) put(key string, value []byte, upd int64) error {
	sealed, err := k.seal(key, value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), k.srv.timeout)
	defer cancel()
	return k.srv.exec(ctx, `INSERT INTO `+Table+` (principal, ns, key, value, upd)
		VALUES ($1, $2, decode($3,'hex'), decode($4,'hex'), $5::bigint)
		ON CONFLICT (principal, ns, key) DO UPDATE SET value = EXCLUDED.value, upd = EXCLUDED.upd`,
		k.principal, k.ns, hx([]byte(key)), hx(sealed), strconv.FormatInt(upd, 10))
}

func (k *sqlKV) del(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), k.srv.timeout)
	defer cancel()
	return k.srv.exec(ctx, `DELETE FROM `+Table+` WHERE principal=$1 AND ns=$2 AND key=decode($3,'hex')`,
		k.principal, k.ns, hx([]byte(key)))
}

// scanPage is how many rows one round trip of an unbounded scan reads.
const scanPage = 1000

func (k *sqlKV) scan(ctx context.Context, prefix, after string, limit int, fn func(key string, value []byte) error) error {
	from, strict := prefix, false
	if after >= prefix {
		from, strict = after, true
	}
	hi := prefixUpperBound(prefix)
	left := limit
	for {
		page := scanPage
		if limit > 0 && left < page {
			page = left
		}
		op := ">="
		if strict {
			op = ">"
		}
		rows, err := k.srv.query(ctx, `SELECT 'h' || encode(key,'hex') AS k, 'h' || encode(value,'hex') AS v FROM `+Table+
			` WHERE principal=$1 AND ns=$2 AND key `+op+` decode($3,'hex') AND key < decode($4,'hex')
			ORDER BY key LIMIT $5::bigint`,
			k.principal, k.ns, hx([]byte(from)), hx([]byte(hi)), strconv.Itoa(page))
		if err != nil {
			return err
		}
		for _, r := range rows {
			key, v, err := k.row(r)
			if err != nil {
				return err
			}
			if err := fn(key, v); err != nil {
				return err
			}
			from, strict = key, true
		}
		if limit > 0 {
			left -= len(rows)
			if left <= 0 {
				return nil
			}
		}
		if len(rows) < page {
			return nil
		}
	}
}

// checkpoint and reclaim are the file's: a server keeps its own pages.
func (k *sqlKV) checkpoint() error                   { return nil }
func (k *sqlKV) reclaim(context.Context, bool) error { return nil }
func (k *sqlKV) close() error                        { return nil }

// errNoMaster refuses a shared store with no key: its rows would be sealed under a
// key this process mints and loses.
var errNoMaster = errors.New("store: a shared SQL store needs the master key its rows are sealed under")

// NewShared opens the manager whose shards are rows of one table on the hanzoai/sql
// server at addr (host:port, its ZAP listener). master is the 32-byte root key
// every row is sealed under, as New's is every file's; it is required.
func NewShared(ctx context.Context, addr string, master []byte) (*Manager, error) {
	if addr == "" {
		return nil, errors.New("store.NewShared: the SQL server's address is required")
	}
	if len(master) != MasterKeyLen {
		return nil, errNoMaster
	}
	srv := dialServer(addr)
	if err := srv.exec(ctx, tableDDL); err != nil {
		srv.close()
		return nil, fmt.Errorf("store.NewShared: %w", err)
	}
	m := &Manager{
		shards: make(map[shardKey]*Shard),
		stopGC: make(chan struct{}),
		srv:    srv,
		master: append([]byte(nil), master...),
	}
	go m.gc()
	return m, nil
}

// sharedNames is every shard the table holds rows for, by principal.
func (m *Manager) sharedNames(ctx context.Context) (map[string][]string, error) {
	rows, err := m.srv.query(ctx, `SELECT DISTINCT principal AS p, ns AS n FROM `+Table+` ORDER BY principal, ns`)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, r := range rows {
		p, err := col(r, "p")
		if err != nil {
			return nil, err
		}
		n, err := col(r, "n")
		if err != nil {
			return nil, err
		}
		out[p] = append(out[p], n)
	}
	return out, nil
}

// sharedPrincipals is every tenant that holds rows in the table, sorted.
func (m *Manager) sharedPrincipals(ctx context.Context) ([]Principal, error) {
	names, err := m.sharedNames(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Principal, 0, len(names))
	for s := range names {
		p, err := ParsePrincipal(s)
		if err != nil {
			return nil, fmt.Errorf("store: the table holds rows for %q: %w", s, err)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// putBatch is how many rows one statement of a copy writes.
const putBatch = 500

// putMany writes rows in statements of putBatch, for a copy.
func (k *sqlKV) putMany(ctx context.Context, keys []string, values [][]byte) error {
	for i := 0; i < len(keys); i += putBatch {
		j := min(i+putBatch, len(keys))
		var sb strings.Builder
		sb.WriteString(`INSERT INTO ` + Table + ` (principal, ns, key, value, upd) VALUES `)
		args := []any{k.principal, k.ns, strconv.FormatInt(now(), 10)}
		for n := i; n < j; n++ {
			sealed, err := k.seal(keys[n], values[n])
			if err != nil {
				return err
			}
			if n > i {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "($1, $2, decode($%d,'hex'), decode($%d,'hex'), $3::bigint)", len(args)+1, len(args)+2)
			args = append(args, hx([]byte(keys[n])), hx(sealed))
		}
		sb.WriteString(` ON CONFLICT (principal, ns, key) DO UPDATE SET value = EXCLUDED.value, upd = EXCLUDED.upd`)
		if err := k.srv.exec(ctx, sb.String(), args...); err != nil {
			return err
		}
	}
	return nil
}
