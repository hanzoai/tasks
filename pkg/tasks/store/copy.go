// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Report is one shard's copy: how many rows it held, how many reached the
// destination, and the digest both sides answered.
type Report struct {
	Principal Principal
	Namespace string
	Rows      int
	Written   int
	Digest    string // over every key and value in key order; equal on both sides when verified
}

// Copy writes every shard src holds into dst, row by row, and then reads both back:
// a shard is copied only when both sides hold the same rows, counted and digested
// in key order. It copies shards whose rows dst does not yet hold, and re-checks the
// ones it does, so a copy that stopped part-way runs again from the start and ends
// where a clean one does. The source is only read; the destination ends holding the
// source's rows and no others.
func Copy(ctx context.Context, src, dst *Manager) ([]Report, error) {
	ps, err := src.ListPrincipals(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.Copy: list: %w", err)
	}
	var out []Report
	for _, p := range ps {
		for _, ns := range src.shardNames(p) {
			r, err := copyShard(ctx, src, dst, p, ns)
			if err != nil {
				return out, err
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func copyShard(ctx context.Context, src, dst *Manager, p Principal, ns string) (Report, error) {
	r := Report{Principal: p, Namespace: ns}
	from, err := src.Get(ctx, p, ns)
	if err != nil {
		return r, fmt.Errorf("store.Copy %s/%s: open source: %w", p, ns, err)
	}
	to, err := dst.Get(ctx, p, ns)
	if err != nil {
		return r, fmt.Errorf("store.Copy %s/%s: open destination: %w", p, ns, err)
	}
	keys, values, held := []string{}, [][]byte{}, map[string]bool{}
	flush := func() error {
		if len(keys) == 0 {
			return nil
		}
		if many, ok := to.rows.(*sqlKV); ok {
			if err := many.putMany(ctx, keys, values); err != nil {
				return err
			}
		} else {
			for i := range keys {
				if err := to.rows.put(keys[i], values[i], now()); err != nil {
					return err
				}
			}
		}
		r.Written += len(keys)
		keys, values = keys[:0], values[:0]
		return nil
	}
	// The source is read a page at a time and each page is written before the next is
	// read, so the source shard's one connection is never held across a write to the
	// destination: a running engine goes on using it between pages.
	for after := ""; ; {
		n := 0
		if err := from.Scan(ctx, "", after, putBatch, func(k string, v []byte) error {
			r.Rows++
			n++
			held[k] = true
			keys, values = append(keys, k), append(values, append([]byte(nil), v...))
			after = k
			return nil
		}); err != nil {
			return r, fmt.Errorf("store.Copy %s/%s: %w", p, ns, err)
		}
		if err := flush(); err != nil {
			return r, fmt.Errorf("store.Copy %s/%s: %w", p, ns, err)
		}
		if n < putBatch {
			break
		}
	}
	// A row the destination holds and the source does not is one the source deleted
	// after an earlier, interrupted copy: the destination is made to hold what the
	// source holds, nothing more.
	var extra []string
	if err := to.Scan(ctx, "", "", 0, func(k string, _ []byte) error {
		if !held[k] {
			extra = append(extra, k)
		}
		return nil
	}); err != nil {
		return r, fmt.Errorf("store.Copy %s/%s: %w", p, ns, err)
	}
	for _, k := range extra {
		if err := to.rows.del(k); err != nil {
			return r, fmt.Errorf("store.Copy %s/%s: %w", p, ns, err)
		}
	}
	a, an, err := digest(ctx, from)
	if err != nil {
		return r, err
	}
	b, bn, err := digest(ctx, to)
	if err != nil {
		return r, err
	}
	if a != b || an != bn {
		return r, fmt.Errorf("store.Copy %s/%s: the source holds %d rows (%s) and the destination %d (%s)", p, ns, an, a, bn, b)
	}
	r.Digest = a
	return r, nil
}

// digest is a shard's rows, in key order, as one SHA-256: each key and value
// length-prefixed, so no two different shards hash alike.
func digest(ctx context.Context, s *Shard) (string, int, error) {
	h := sha256.New()
	n := 0
	var l [8]byte
	err := s.Scan(ctx, "", "", 0, func(k string, v []byte) error {
		binary.BigEndian.PutUint64(l[:], uint64(len(k)))
		h.Write(l[:])
		h.Write([]byte(k))
		binary.BigEndian.PutUint64(l[:], uint64(len(v)))
		h.Write(l[:])
		h.Write(v)
		n++
		return nil
	})
	if err != nil {
		return "", 0, fmt.Errorf("store: digest %s/%s: %w", s.principal, s.ns, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
