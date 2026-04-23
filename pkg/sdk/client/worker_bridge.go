// Copyright © 2026 Hanzo AI. MIT License.

package client

// TransportOf extracts the underlying Transport from a Client returned
// by Dial. pkg/sdk/worker calls this to layer a typed WorkerTransport
// over the generic Transport the Client already owns — workers share
// the ZAP connection and long-poll channel with the Client so users
// pay for one node per process.
//
// Callers that injected a custom Transport via Options.Transport
// receive the same value they passed in.
//
// Returns nil if c was not produced by Dial (future Client
// implementations outside this package simply opt out of worker
// sharing).
func TransportOf(c Client) Transport {
	if ci, ok := c.(*clientImpl); ok {
		return ci.transport
	}
	return nil
}

// NamespaceOf returns the namespace a Client was dialed with. Workers
// use it as the default namespace for poll RPCs if the caller's
// worker.Options doesn't override.
func NamespaceOf(c Client) string {
	if ci, ok := c.(*clientImpl); ok {
		return ci.namespace
	}
	return ""
}

// IdentityOf returns the identity a Client was dialed with.
func IdentityOf(c Client) string {
	if ci, ok := c.(*clientImpl); ok {
		return ci.identity
	}
	return ""
}
