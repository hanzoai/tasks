// Copyright © 2026 Hanzo AI. MIT License.

// Package auth — identity-header middleware. The trust boundary is the
// IAM JWT: tasksd validates every Authorization: Bearer <jwt> against
// JWKS, mints X-Org-Id / X-User-Id / X-User-Email from validated claims,
// and unconditionally strips any client-supplied identity headers. There
// is no header-pass-through trust path; client-supplied identity headers
// are never honored.
//
// In dev / embedded use, set TASKSD_REQUIRE_IDENTITY=false (the default)
// — requests without a token pass through with empty identity context.
// In production, set TASKSD_REQUIRE_IDENTITY=true so unauthenticated
// requests get 401.
package auth

import (
	"context"
	"net/http"
)

const (
	HeaderOrgID     = "X-Org-Id"
	HeaderUserID    = "X-User-Id"
	HeaderUserEmail = "X-User-Email"

	HeaderAuthorization = "Authorization"
)

type ctxKey int

const (
	ctxKeyOrgID ctxKey = iota
	ctxKeyUserID
	ctxKeyUserEmail
)

// mintedHeaders are the identity headers tasksd considers authoritative
// only when minted from a validated JWT. They are stripped from every
// inbound request before any downstream code sees them.
var mintedHeaders = []string{HeaderOrgID, HeaderUserID, HeaderUserEmail}

// stripIdentityHeaders deletes every minted identity header. Called
// unconditionally on every request — the strip-list ⊇ mint-list contract
// guarantees no client-supplied identity ever reaches the handler.
func stripIdentityHeaders(h http.Header) {
	for _, k := range mintedHeaders {
		h.Del(k)
	}
}

// RequireIdentity returns middleware that:
//  1. Strips any client-supplied X-Org-Id / X-User-Id / X-User-Email.
//  2. If a Bearer JWT is present, validates it via v and mints fresh
//     identity headers + ctx values from the claims.
//  3. If require=true and no validated identity emerged, returns 401.
//
// When v is nil (JWT disabled, embedded/dev mode) and require=false,
// every request passes through with empty identity ctx — useful for
// tests and the in-process embedder. When v is nil and require=true,
// every request is rejected (closed-by-default).
func RequireIdentity(v *Validator, require bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stripIdentityHeaders(r.Header)

			var (
				org, user, email string
				authed           bool
			)
			if v != nil {
				if bearer := r.Header.Get(HeaderAuthorization); bearer != "" {
					claims, err := v.Validate(r.Context(), bearer)
					if err == nil && claims != nil {
						org = claims.Owner
						user = claims.Subject
						if user == "" {
							user = claims.PreferredUsername
						}
						email = claims.Email
						authed = true
						r.Header.Set(HeaderOrgID, org)
						r.Header.Set(HeaderUserID, user)
						if email != "" {
							r.Header.Set(HeaderUserEmail, email)
						}
					}
				}
			}

			if require && !authed {
				http.Error(w, `{"error":"identity required","code":401}`, http.StatusUnauthorized)
				return
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxKeyOrgID, org)
			ctx = context.WithValue(ctx, ctxKeyUserID, user)
			ctx = context.WithValue(ctx, ctxKeyUserEmail, email)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// OrgID returns the org id minted from a validated JWT, or "".
func OrgID(ctx context.Context) string { return strFromCtx(ctx, ctxKeyOrgID) }

// UserID returns the user id minted from a validated JWT, or "".
func UserID(ctx context.Context) string { return strFromCtx(ctx, ctxKeyUserID) }

// UserEmail returns the user email minted from a validated JWT, or "".
func UserEmail(ctx context.Context) string { return strFromCtx(ctx, ctxKeyUserEmail) }

func strFromCtx(ctx context.Context, k ctxKey) string {
	if v, ok := ctx.Value(k).(string); ok {
		return v
	}
	return ""
}
