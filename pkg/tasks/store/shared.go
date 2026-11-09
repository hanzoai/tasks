// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Shared keeps every tenant's rows in the tables of ONE database that several
// replicas open at once, where Manager keeps them in a file per (tenant,
// namespace) that one process owns.
//
// The engine's rows are a keyed document store (see Shard), and Shared is that
// store's second home: the same keys, the same values, the same prefix scans, in
//
//	tasks_kv(kind, principal, key, value, upd)  primary key (kind, principal, key)
//
// A key already carries its namespace as its second segment, so there is no
// namespace column to keep in step with it. Its first segment is the kind
// ("wf", "sc", "wfh", ...), made a column so that the one scan which has to
// cross tenants, every schedule of every org, is a range over one kind and not
// a walk of every history row ever written.
//
// The database is the handle the host passes in, on either engine. The SQL is the
// dialect PostgreSQL and SQLite both read: numbered $n placeholders, ON CONFLICT,
// integers for times. What they do not agree on is the column type of a blob and
// the collation of text, and those two are read off the handle's driver here.
//
// Beside the rows, Shared keeps two small tables the replicas need to agree on
// something: tasks_lease, a row per name with a holder and an expiry in the
// DATABASE'S clock (membership, leadership and the short locks around a run), and
// tasks_meta (what has been moved in from files). Time is the database's, never a
// replica's, so a replica with a slow clock can neither keep a lease it lost nor
// take one that is not yet over.
//
// The handle belongs to the caller: Shared migrates it, uses it and does not close
// it.
type Shared struct {
	db     *sql.DB
	pg     bool
	closed atomic.Bool
}

// ErrKey is the refusal of a key the database cannot hold.
var ErrKey = errors.New("store: a key must be valid UTF-8 without NUL, and name its kind (kind/...)")

// OpenShared migrates db and returns the store on it. Two replicas opening one
// database at once is how a deployment starts, so the migration is serialised on
// PostgreSQL by a transaction-scoped advisory lock, and each statement is safe to
// run again.
func OpenShared(ctx context.Context, db *sql.DB) (*Shared, error) {
	if db == nil {
		return nil, errors.New("store.OpenShared: a database is required")
	}
	s := &Shared{db: db, pg: postgres(db)}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.OpenShared: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.pg {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':tasks:migrate', 0))`); err != nil {
			return nil, fmt.Errorf("store.OpenShared: lock: %w", err)
		}
	}
	for _, ddl := range s.schema() {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return nil, fmt.Errorf("store.OpenShared: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.OpenShared: %w", err)
	}
	return s, nil
}

// postgres reads the engine off the handle's driver. The package it is built in
// is the whole of the answer; this module links no PostgreSQL driver of its own.
func postgres(db *sql.DB) bool {
	t := reflect.TypeOf(db.Driver())
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t != nil && strings.HasPrefix(t.PkgPath(), "github.com/jackc/pgx/")
}

func (s *Shared) schema() []string {
	blob, text := "BLOB", "TEXT"
	if s.pg {
		// A prefix range and ORDER BY on text follow the database's collation on
		// PostgreSQL and the bytes on SQLite. The keys are ordered by their bytes.
		blob, text = "BYTEA", `TEXT COLLATE "C"`
	}
	return []string{
		`CREATE TABLE IF NOT EXISTS tasks_kv (
			kind      ` + text + ` NOT NULL,
			principal ` + text + ` NOT NULL,
			key       ` + text + ` NOT NULL,
			value     ` + blob + ` NOT NULL,
			upd       BIGINT NOT NULL,
			PRIMARY KEY (kind, principal, key)
		)`,
		`CREATE TABLE IF NOT EXISTS tasks_lease (
			name    ` + text + ` NOT NULL PRIMARY KEY,
			holder  TEXT NOT NULL,
			expires BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tasks_meta (
			key   ` + text + ` NOT NULL PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
}

// Postgres reports whether the database is PostgreSQL.
func (s *Shared) Postgres() bool { return s.pg }

// Close ends the store. The database handle is the caller's and stays open.
func (s *Shared) Close() error {
	s.closed.Store(true)
	return nil
}

// kindOf is the first segment of a key or of a prefix that reaches past it.
func kindOf(s string) (string, bool) {
	i := strings.IndexByte(s, '/')
	return s[:max(i, 0)], i > 0
}

func (s *Shared) check(key string) (string, error) {
	if s.closed.Load() {
		return "", ErrClosed
	}
	kind, ok := kindOf(key)
	if !ok || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
		return "", fmt.Errorf("%w: %q", ErrKey, key)
	}
	return kind, nil
}

// Put writes value at key for p.
func (s *Shared) Put(ctx context.Context, p Principal, key string, value []byte) error {
	kind, err := s.check(key)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO tasks_kv (kind, principal, key, value, upd) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (kind, principal, key) DO UPDATE SET value = excluded.value, upd = excluded.upd`,
		kind, p.String(), key, value, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("store.Put(%s): %w", key, err)
	}
	return nil
}

// Get reads key for p.
func (s *Shared) Get(ctx context.Context, p Principal, key string) ([]byte, bool, error) {
	kind, err := s.check(key)
	if err != nil {
		return nil, false, err
	}
	var v []byte
	err = s.db.QueryRowContext(ctx, `SELECT value FROM tasks_kv WHERE kind = $1 AND principal = $2 AND key = $3`,
		kind, p.String(), key).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("store.Get(%s): %w", key, err)
	}
	return v, true, nil
}

// Del removes key for p. Missing is not an error.
func (s *Shared) Del(ctx context.Context, p Principal, key string) error {
	kind, err := s.check(key)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tasks_kv WHERE kind = $1 AND principal = $2 AND key = $3`,
		kind, p.String(), key); err != nil {
		return fmt.Errorf("store.Del(%s): %w", key, err)
	}
	return nil
}

// Scan walks, in key order, at most limit of p's rows (all of them when limit <= 0)
// whose key starts with prefix and sorts after `after`. The rows of the page are
// read before fn sees the first, so fn may ask the store for more: on SQLite the
// handle holds one connection, and a callback that queried it while the page was
// still being read would wait for itself.
func (s *Shared) Scan(ctx context.Context, p Principal, prefix, after string, limit int, fn func(key string, value []byte) error) error {
	kind, err := s.check(prefix)
	if err != nil {
		return err
	}
	from, op := prefix, ">="
	if after >= prefix {
		from, op = after, ">"
	}
	q := `SELECT key, value FROM tasks_kv WHERE kind = $1 AND principal = $2 AND key ` + op + ` $3 AND key < $4 ORDER BY key`
	args := []any{kind, p.String(), from, upperBound(prefix)}
	if limit > 0 {
		q += ` LIMIT $5`
		args = append(args, limit)
	}
	type row struct {
		key   string
		value []byte
	}
	var rows []row
	r, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store.Scan(%s): %w", prefix, err)
	}
	for r.Next() {
		var x row
		if err := r.Scan(&x.key, &x.value); err != nil {
			_ = r.Close()
			return err
		}
		rows = append(rows, x)
	}
	if err := errors.Join(r.Err(), r.Close()); err != nil {
		return fmt.Errorf("store.Scan(%s): %w", prefix, err)
	}
	for _, x := range rows {
		if err := fn(x.key, x.value); err != nil {
			return err
		}
	}
	return nil
}

// List walks every row of p whose key starts with prefix.
func (s *Shared) List(ctx context.Context, p Principal, prefix string, fn func(key string, value []byte) error) error {
	return s.Scan(ctx, p, prefix, "", 0, fn)
}

// ScanAll walks every tenant's rows whose key starts with prefix, the tenant
// beside each. It is the cron sweeper's view of the world, and it is one range
// over one kind, so it costs what the kind holds and not what the database does.
func (s *Shared) ScanAll(ctx context.Context, prefix string, fn func(p Principal, key string, value []byte) error) error {
	kind, err := s.check(prefix)
	if err != nil {
		return err
	}
	r, err := s.db.QueryContext(ctx, `SELECT principal, key, value FROM tasks_kv WHERE kind = $1 AND key >= $2 AND key < $3 ORDER BY principal, key`,
		kind, prefix, upperBound(prefix))
	if err != nil {
		return fmt.Errorf("store.ScanAll(%s): %w", prefix, err)
	}
	type row struct {
		principal, key string
		value          []byte
	}
	var rows []row
	for r.Next() {
		var x row
		if err := r.Scan(&x.principal, &x.key, &x.value); err != nil {
			_ = r.Close()
			return err
		}
		rows = append(rows, x)
	}
	if err := errors.Join(r.Err(), r.Close()); err != nil {
		return fmt.Errorf("store.ScanAll(%s): %w", prefix, err)
	}
	for _, x := range rows {
		p, err := ParsePrincipal(x.principal)
		if err != nil {
			continue // a row no engine view wrote; never poison a sweep with it
		}
		if err := fn(p, x.key, x.value); err != nil {
			return err
		}
	}
	return nil
}

// Principals lists every tenant that has registered a namespace, sorted.
func (s *Shared) Principals(ctx context.Context) ([]Principal, error) {
	r, err := s.db.QueryContext(ctx, `SELECT DISTINCT principal FROM tasks_kv WHERE kind = 'ns' ORDER BY principal`)
	if err != nil {
		return nil, fmt.Errorf("store.Principals: %w", err)
	}
	var names []string
	for r.Next() {
		var n string
		if err := r.Scan(&n); err != nil {
			_ = r.Close()
			return nil, err
		}
		names = append(names, n)
	}
	if err := errors.Join(r.Err(), r.Close()); err != nil {
		return nil, fmt.Errorf("store.Principals: %w", err)
	}
	var out []Principal
	for _, n := range names {
		if p, err := ParsePrincipal(n); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// upperBound is the smallest string above every string that starts with prefix. A
// last byte that can be incremented stays valid UTF-8 (the keys are ASCII where it
// matters: they end in a slash), and one that cannot is closed with the largest
// code point there is.
func upperBound(prefix string) string {
	if n := len(prefix); n > 0 && prefix[n-1] < 0x7f {
		return prefix[:n-1] + string(prefix[n-1]+1)
	}
	return prefix + "\U0010FFFF"
}

// now is the database's clock in whole seconds.
func (s *Shared) now() string {
	if s.pg {
		return `extract(epoch FROM now())::bigint`
	}
	return `unixepoch()`
}

// Claim takes, or renews, holder's lease on name for ttl, and reports whether
// holder has it. A lease another holder has is not taken until its expiry has
// passed on the DATABASE's clock. The ask is a plain SELECT first, because an
// INSERT ... ON CONFLICT locks the row it conflicts with and burns a transaction id
// even when its WHERE refuses it, and a replica that does not hold a lease asks for
// it every few seconds.
func (s *Shared) Claim(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	var owner string
	var left int64
	err := s.db.QueryRowContext(ctx, `SELECT holder, expires - `+s.now()+` FROM tasks_lease WHERE name = $1`, name).Scan(&owner, &left)
	switch {
	case err == nil && owner != holder && left > 0:
		return false, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("store.Claim(%s): %w", name, err)
	}
	var expires int64
	err = s.db.QueryRowContext(ctx, `INSERT INTO tasks_lease AS l (name, holder, expires) VALUES ($1, $2, `+s.now()+` + $3)
		ON CONFLICT (name) DO UPDATE SET holder = excluded.holder, expires = excluded.expires
		WHERE l.holder = excluded.holder OR l.expires < `+s.now()+`
		RETURNING expires`, name, holder, int64(ttl/time.Second)).Scan(&expires)
	switch {
	case errors.Is(err, sql.ErrNoRows): // another holder took it between the look and the write
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store.Claim(%s): %w", name, err)
	}
	return true, nil
}

// Release gives up holder's lease on name, now and not at its expiry. A lease the
// holder no longer has (it lapsed and was taken) is left to its new holder.
func (s *Shared) Release(ctx context.Context, name, holder string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tasks_lease WHERE name = $1 AND holder = $2`, name, holder); err != nil {
		return fmt.Errorf("store.Release(%s): %w", name, err)
	}
	return nil
}

// Expire ends holder's lease on name now, leaving the row: its term is over, so it
// reads as lapsed to whoever looks next. It is how a member leaves, as opposed to
// a lock being released, which leaves nothing.
func (s *Shared) Expire(ctx context.Context, name, holder string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks_lease SET expires = `+s.now()+` - 1 WHERE name = $1 AND holder = $2`, name, holder); err != nil {
		return fmt.Errorf("store.Expire(%s): %w", name, err)
	}
	return nil
}

// Held lists the names under prefix whose lease is still in force, and Lapsed the
// ones whose term is over and that nobody has released: a holder that died.
func (s *Shared) Held(ctx context.Context, prefix string) ([]string, error) {
	return s.names(ctx, prefix, `>=`)
}

// Lapsed: see Held.
func (s *Shared) Lapsed(ctx context.Context, prefix string) ([]string, error) {
	return s.names(ctx, prefix, `<`)
}

func (s *Shared) names(ctx context.Context, prefix, cmp string) ([]string, error) {
	r, err := s.db.QueryContext(ctx, `SELECT name FROM tasks_lease WHERE name >= $1 AND name < $2 AND expires `+cmp+` `+s.now()+` ORDER BY name`,
		prefix, upperBound(prefix))
	if err != nil {
		return nil, fmt.Errorf("store.Held(%s): %w", prefix, err)
	}
	var out []string
	for r.Next() {
		var n string
		if err := r.Scan(&n); err != nil {
			_ = r.Close()
			return nil, err
		}
		out = append(out, n)
	}
	if err := errors.Join(r.Err(), r.Close()); err != nil {
		return nil, fmt.Errorf("store.Held(%s): %w", prefix, err)
	}
	return out, nil
}

// Forget drops every lease under prefix whose term ended before the given age, so
// the names of locks taken once do not accumulate.
func (s *Shared) Forget(ctx context.Context, prefix string, age time.Duration) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tasks_lease WHERE name >= $1 AND name < $2 AND expires < `+s.now()+` - $3`,
		prefix, upperBound(prefix), int64(age/time.Second))
	if err != nil {
		return fmt.Errorf("store.Forget(%s): %w", prefix, err)
	}
	return nil
}

// Meta reads a note the store keeps about itself.
func (s *Shared) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM tasks_meta WHERE key = $1`, key).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("store.Meta(%s): %w", key, err)
	}
	return v, true, nil
}

// SetMeta writes a note the store keeps about itself.
func (s *Shared) SetMeta(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks_meta (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("store.SetMeta(%s): %w", key, err)
	}
	return nil
}

// Row is one key and its value.
type Row struct {
	Key   string
	Value []byte
}

// putChunk is how many rows one statement writes: five parameters a row, well under
// the 65535 a statement may bind.
const putChunk = 200

// PutMany writes rows for p in one transaction, a statement to every putChunk rows. A
// key already there is replaced. It is how rows are moved in from files, where one
// round trip a row would be hours.
func (s *Shared) PutMany(ctx context.Context, p Principal, rows []Row) error {
	if s.closed.Load() {
		return ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.PutMany: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()
	for len(rows) > 0 {
		n := min(len(rows), putChunk)
		var b strings.Builder
		b.WriteString(`INSERT INTO tasks_kv (kind, principal, key, value, upd) VALUES `)
		args := make([]any, 0, n*5)
		for i, r := range rows[:n] {
			kind, err := s.check(r.Key)
			if err != nil {
				return err
			}
			if i > 0 {
				b.WriteString(", ")
			}
			base := i * 5
			fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d)", base+1, base+2, base+3, base+4, base+5)
			args = append(args, kind, p.String(), r.Key, r.Value, now)
		}
		b.WriteString(` ON CONFLICT (kind, principal, key) DO UPDATE SET value = excluded.value, upd = excluded.upd`)
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return fmt.Errorf("store.PutMany: %w", err)
		}
		rows = rows[n:]
	}
	return tx.Commit()
}

// Digest is how many rows p has and a sum over them that does not depend on the order
// they are read in: the two ends of a move agree on it when they hold the same rows.
func (s *Shared) Digest(ctx context.Context, p Principal) (n int64, sum uint64, err error) {
	r, err := s.db.QueryContext(ctx, `SELECT key, value FROM tasks_kv WHERE principal = $1`, p.String())
	if err != nil {
		return 0, 0, fmt.Errorf("store.Digest: %w", err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
		var k string
		var v []byte
		if err := r.Scan(&k, &v); err != nil {
			return 0, 0, err
		}
		n, sum = n+1, sum+RowSum(k, v)
	}
	return n, sum, r.Err()
}

// RowSum is one row's part of a Digest.
func RowSum(key string, value []byte) uint64 {
	h := sha256.New()
	h.Write([]byte(key))
	h.Write([]byte{0})
	h.Write(value)
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}
