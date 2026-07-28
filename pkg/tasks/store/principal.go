// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"fmt"
	"path/filepath"
	"strings"

	hanzosqlite "github.com/hanzoai/sqlite"
)

// Sentinel is the path segment standing for an unset leg of a Principal.
// It is not a legal id, so a Principal with an unset leg can never be
// confused with one that names a tenant of that name.
const Sentinel = "_"

// Depth is the number of legs in a Principal: org, project, user.
const Depth = 3

// Principal is the tenant that owns a shard: the org, optionally narrowed
// to a project and to a user. It is the SAME value in both places tenancy
// is decided, so the two can never disagree:
//
//   - the directory the shard's file lives in (String), and
//   - the key-encryption key that file's DEK is wrapped under (KEK).
//
// Legs narrow left to right and each unset leg is written as Sentinel, so
// the encoding is fixed-width and injective: acme/_/z (an org's user, no
// project) and acme/z/_ (an org's project) are distinct paths and derive
// distinct keys. The zero Principal is the root — the unscoped embedded /
// dev tenant.
//
// The org is what a tenant IS; project and user narrow it and are each
// independently optional, because IAM mints identities that carry a user
// without a project. Nothing may be set without an org.
type Principal struct {
	Org     string
	Project string
	User    string
}

// Org returns the principal naming an org.
func Org(org string) Principal { return Principal{Org: org} }

// legs returns the three legs in narrowing order.
func (p Principal) legs() [Depth]string { return [Depth]string{p.Org, p.Project, p.User} }

// Root reports whether p names no tenant at all.
func (p Principal) Root() bool { return p == Principal{} }

// String is the canonical encoding: the three legs joined by "/", each
// unset leg written as Sentinel. It is BOTH the shard's directory
// (relative to the store root) and the routing key carried on a
// replication frame — one encoding, so a frame always lands in the
// directory its principal names.
func (p Principal) String() string {
	legs := p.legs()
	out := make([]string, Depth)
	for i, leg := range legs {
		if leg == "" {
			out[i] = Sentinel
			continue
		}
		out[i] = leg
	}
	return strings.Join(out, "/")
}

// dir is the shard directory for p relative to the store root.
func (p Principal) dir() string { return filepath.FromSlash(p.String()) }

// ParsePrincipal is the inverse of String.
func ParsePrincipal(s string) (Principal, error) {
	legs := strings.Split(s, "/")
	if len(legs) != Depth {
		return Principal{}, fmt.Errorf("store: principal %q must have %d legs", s, Depth)
	}
	for i, leg := range legs {
		if leg == Sentinel {
			legs[i] = ""
		}
	}
	p := Principal{Org: legs[0], Project: legs[1], User: legs[2]}
	if err := p.Valid(); err != nil {
		return Principal{}, err
	}
	return p, nil
}

// Valid reports whether p is well formed: every set leg is a usable path
// segment, and nothing is set without an org to narrow.
func (p Principal) Valid() error {
	legs := p.legs()
	if legs[0] == "" && (legs[1] != "" || legs[2] != "") {
		return fmt.Errorf("store: principal %+v narrows an unset org", p)
	}
	for _, leg := range legs {
		if leg == "" {
			continue
		}
		if err := ValidName(leg); err != nil {
			return err
		}
	}
	return nil
}

// ValidName rejects anything that would escape or alias a shard directory
// or file, or collide with the unset-leg sentinel. Every name that becomes
// a path segment — a principal's legs and a namespace alike — passes
// through here, so there is one rule for all of them.
func ValidName(name string) error {
	switch name {
	case "":
		return fmt.Errorf("store: name required")
	case Sentinel:
		return fmt.Errorf("store: %q is the unset-leg sentinel and cannot name a tenant", name)
	case ".", "..":
		return fmt.Errorf("store: %q is not a name", name)
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("store: name %q contains a path separator", name)
	}
	return nil
}

// principalProject is the domain-separation tag for the project leg.
// hanzoai/sqlite ships global/org/user; project is the third leg IAM
// already validates and mints (auth.Claims.Project), so it needs its own
// tag for the HKDF info to stay injective across legs.
const principalProject = hanzosqlite.PrincipalType("project")

// rootPrincipalID is the id the root (zero) Principal derives under. The
// root tenant is the service itself, so it derives as the global
// principal — distinct from any org, project or user that could be named
// "tasks".
const rootPrincipalID = "tasks"

// KEK derives the key-encryption key for p from the master key by walking
// the principal chain: master → org → project → user, skipping any leg p
// leaves unset. Each narrowing step derives from the key it narrows, so a
// user's key is cryptographically contained in its org's — reaching a
// user's data requires the whole chain, not just the master. Each step
// carries its own domain-separation tag, so narrowing to a project named
// "z" and to a user named "z" derive different keys.
//
// The KEK never encrypts a page. It wraps that shard's own DEK (see
// dek.go), which is what makes master-key rotation O(1) per shard.
func (p Principal) KEK(master []byte) ([]byte, error) {
	if err := p.Valid(); err != nil {
		return nil, err
	}
	if p.Root() {
		return hanzosqlite.DeriveKey(master, hanzosqlite.PrincipalGlobal, rootPrincipalID)
	}
	key, err := hanzosqlite.DeriveKey(master, hanzosqlite.PrincipalOrg, p.Org)
	if err != nil {
		return nil, err
	}
	for _, leg := range []struct {
		typ hanzosqlite.PrincipalType
		id  string
	}{
		{principalProject, p.Project},
		{hanzosqlite.PrincipalUser, p.User},
	} {
		if leg.id == "" {
			continue
		}
		if key, err = hanzosqlite.DeriveChildKey(key, leg.typ, leg.id); err != nil {
			return nil, err
		}
	}
	return key, nil
}
