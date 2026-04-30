// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"strings"
)

// nsFromKey parses the canonical key layout to derive the namespace
// segment. Returns ("", false) for keys that are themselves the
// namespace registry (`ns/<name>`); the caller treats those specially.
//
// Layout reference:
//
//	ns/<name>
//	wf/<ns>/<workflowId>/<runId>
//	wfh/<ns>/<workflowId>/<runId>/<eventId>
//	sc/<ns>/<scheduleId>
//	bt/<ns>/<batchId>
//	dp/<ns>/<deploymentName>
//	nx/<ns>/<endpointName>
//	id/<ns>/<email>
//	sa/<ns>/<attrName>
//	idem/<ns>/<workflowId>/<requestId>
func NsFromKey(key string) (kind, ns, rest string, ok bool) {
	slash := strings.IndexByte(key, '/')
	if slash <= 0 {
		return "", "", "", false
	}
	kind = key[:slash]
	tail := key[slash+1:]
	if kind == "ns" {
		return kind, tail, "", true
	}
	slash2 := strings.IndexByte(tail, '/')
	if slash2 <= 0 {
		return kind, tail, "", true
	}
	ns = tail[:slash2]
	rest = tail[slash2+1:]
	return kind, ns, rest, true
}

// CrossNamespaceKinds lists the kinds whose List(prefix) operations
// must enumerate every shard under the org. Anything else routes to a
// single shard.
var CrossNamespaceKinds = map[string]bool{
	"ns": true,
	"nx": true, // ListAllNexusEndpoints uses bare "nx/" prefix
}

// IsCrossNamespacePrefix reports whether prefix is a bare kind/ scan
// that must fan out across shards.
func IsCrossNamespacePrefix(prefix string) bool {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	parts := strings.SplitN(prefix, "/", 3)
	if len(parts) < 2 {
		return false
	}
	if !CrossNamespaceKinds[parts[0]] {
		return false
	}
	// "ns/" scans every namespace; "ns/foo/" is a single key.
	return parts[1] == ""
}

// SplitPrefix returns (kind, ns, suffix) for a list prefix. ns may be
// empty when IsCrossNamespacePrefix(prefix) is true.
func SplitPrefix(prefix string) (kind, ns, suffix string) {
	parts := strings.SplitN(prefix, "/", 3)
	if len(parts) >= 1 {
		kind = parts[0]
	}
	if len(parts) >= 2 {
		ns = parts[1]
	}
	if len(parts) >= 3 {
		suffix = parts[2]
	}
	return
}
