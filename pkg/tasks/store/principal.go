// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hanzoai/namespace"
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
//
// It is THE DOOR: the raw IAM org a caller holds is folded here, once, through
// namespace.Sanitize — the one injective slugger — so everything downstream
// carries the STORAGE identity. That matters because a Principal is both the
// routing key on a replication frame and the name of the directory its shard
// lives in: folding again at the open would re-suffix a slug that already
// carries a disambiguation suffix, and land the tenant in a different, empty
// database than the one ListPrincipals just read off disk.
//
// A name Sanitize REFUSES — it carries whitespace, a control or a format rune,
// the class no injective fold survives — is kept RAW rather than folded, so
// that Valid rejects it. Folding it would produce the empty string, and an
// empty org leg is the ROOT principal: a name too hostile to store would have
// become the deployment's own platform tenant. Fail-closed, loudly.
func Org(org string) Principal { return Principal{Org: fold(org)} }

// OrgProject is the door for a principal narrowed to one of an org's
// projects. Both legs are folded, by the same rule, in the same place.
func OrgProject(org, project string) Principal {
	return Principal{Org: fold(org), Project: fold(project)}
}

// fold reduces one externally-chosen name to its storage identity.
//
// A name Sanitize REFUSES is returned RAW rather than folded, so that Valid
// rejects it. Sanitize's refusal is the empty string, and an empty leg means
// "unset" — an unset org leg is the ROOT principal — so folding a hostile
// name would silently hand it the deployment's own platform tenant instead
// of turning it away. An empty input is genuinely unset and stays so.
func fold(name string) string {
	if name == "" {
		return ""
	}
	if slug := namespace.Sanitize(name); slug != "" {
		return slug
	}
	return name
}

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
//
// The rule is hanzoai/namespace's own segment rule, asked rather than
// restated: a name must already BE a legal namespace segment, exactly as
// written. That settles what a name cannot be by construction instead of by
// denylist — no separator so no path, no dot so no ".." and no hidden name,
// no trailing space, no encoded form of any of those — and it holds for the
// namespace leg too, which cek renders straight into a filename.
//
// "Exactly as written" is the strict part, and it is deliberate. The
// namespace constructor case-folds, so it would ACCEPT "Acme" and quietly
// store it as "acme"; a leg that only becomes legal after folding has two
// spellings, and the second one names the same file under a different key.
// Legs arrive already folded (see Org), so anything that would change here
// did not come through the door.
func ValidName(name string) error {
	switch name {
	case "":
		return fmt.Errorf("store: name required")
	case Sentinel:
		return fmt.Errorf("store: %q is the unset-leg sentinel and cannot name a tenant", name)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("store: name %q contains a NUL", name)
	}
	ns, err := namespace.Org(name)
	if err != nil {
		return fmt.Errorf("store: %q cannot name a shard: %w", name, err)
	}
	if ns.ID() != name {
		return fmt.Errorf("store: name %q is not in its stored form (%q); it did not come through the door", name, ns.ID())
	}
	return nil
}

// ErrUserLeg reports that a Principal narrows to a USER, which the shared
// namespace layout has no place for.
//
// hanzoai/namespace names an org, an org's project, and the deployment
// itself. namespace.Key returns an error for a user namespace on purpose —
// "this layout has no place for them, and inventing one silently is how a
// second convention starts". Tasks used to write <root>/<org>/<project>/<user>/,
// a layout of its own; it now shares the estate's, and the user leg is the
// one part of its tenancy that does not survive the move.
//
// It is refused at the door rather than only when the file is keyed, so a
// user-scoped shard fails the same way in dev as in production instead of
// working plaintext and dying the day a master key is set.
var ErrUserLeg = errors.New("store: the shared namespace layout has no place for a user-scoped shard; scope to the org or its project")

// Namespace is the entity hanzoai/namespace names this shard's tenant, and
// therefore both where its file lives and what its key is derived from —
// one name, two renderings, so the two cannot drift apart:
//
//	root (zero)        →  system            →  orgs/_platform/<ns>.db
//	{Org}              →  org/<org>         →  orgs/<org>/<ns>.db
//	{Org, Project}     →  org/<org>/<proj>  →  orgs/<org>/projects/<proj>/<ns>.db
//	anything with User →  ErrUserLeg
//
// The legs are already the folded storage identity (see Org), so this
// VALIDATES them and does not fold again — namespace.Sanitize is injective
// but not idempotent, and a second fold would rename a tenant whose first
// fold carried a disambiguation suffix.
func (p Principal) Namespace() (namespace.Namespace, error) {
	if err := p.Valid(); err != nil {
		return namespace.Namespace{}, err
	}
	if p.User != "" {
		return namespace.Namespace{}, fmt.Errorf("%w: %s", ErrUserLeg, p)
	}
	if p.Root() {
		return namespace.System(), nil
	}
	ns, err := namespace.Org(p.Org)
	if err != nil {
		return namespace.Namespace{}, err
	}
	if p.Project == "" {
		return ns, nil
	}
	g, err := namespace.NewGroup(p.Project)
	if err != nil {
		return namespace.Namespace{}, fmt.Errorf("store: project %q: %w", p.Project, err)
	}
	return ns.WithGroup(g), nil
}
