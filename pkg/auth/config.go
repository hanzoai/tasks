// Copyright © 2026 Hanzo AI. MIT License.

// The credential policy, from tasksd's own environment.
//
// The CHECK itself — keys, issuer, audience, the header contract — is
// hanzoai/authz/edge. This file is only the part that is genuinely tasksd's: which
// environment variables the values come from.
//
// A reader used to live here, and it had two defects a shared one does not: it
// verified against EVERY key in the published set rather than the one the token's
// `kid` named, so a token naming key A was accepted on a signature from key B; and
// its issuer comparison was conditional on the configured issuer being non-empty, so
// an unset TASKSD_JWT_ISSUER accepted tokens from anyone instead of failing closed.
package auth

import (
	"time"

	"github.com/hanzoai/authz/edge"
)

// JWTConfig configures the validator. An empty JWKSURL disables JWT validation, and
// RequireIdentity then refuses every request when require is set.
type JWTConfig struct {
	JWKSURL  string        // e.g. https://hanzo.id/v1/iam/.well-known/jwks
	Issuer   string        // e.g. https://hanzo.id
	Audience string        // optional; "" → audience check skipped
	TTL      time.Duration // JWKS cache TTL; 0 → the edge's default
}

// NewValidator returns nil when cfg names no JWKS URL (JWT disabled).
//
// The audience is optional here and that is deliberate, not an oversight: IAM sets
// `aud` per RFC 8707 to the requesting CLIENT, so it names who asked for the token
// rather than who may accept it. tasksd is not a client id, so it pins the issuer and
// the signing keys and lets any first-party client's token through.
func NewValidator(cfg JWTConfig) *edge.Verifier {
	if cfg.JWKSURL == "" {
		return nil
	}
	var audiences []string
	if cfg.Audience != "" {
		audiences = []string{cfg.Audience}
	}
	return edge.NewVerifier(cfg.JWKSURL, cfg.Issuer, audiences, cfg.TTL)
}
