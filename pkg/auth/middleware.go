// Copyright © 2026 Hanzo AI. MIT License.

// Package auth — identity-header middleware. The trust boundary is the
// IAM JWT: tasksd validates every Authorization: Bearer <jwt> against
// JWKS, writes X-Org-Id / X-User-Id / X-User-Email from validated claims,
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

	"github.com/hanzoai/authz"
	"github.com/hanzoai/authz/edge"
)

// The identity header names are the ESTATE's, not this service's: one list, named
// by the party that writes their values (hanzoai/authz), so tasksd cannot come to
// disagree with the edge about what X-Org-Id means.
const (
	HeaderOrgID     = authz.HeaderOrg
	HeaderProjectID = authz.HeaderProject
	HeaderUserID    = authz.HeaderUser
	HeaderUserEmail = authz.HeaderUserEmail

	HeaderAuthorization = "Authorization"
)

type ctxKey int

const (
	ctxKeyOrgID ctxKey = iota
	ctxKeyProjectID
	ctxKeyUserID
	ctxKeyUserEmail
)

// RequireIdentity returns middleware that:
//  1. Strips any client-supplied X-Org-Id / X-User-Id / X-User-Email.
//  2. If a Bearer JWT is present, validates it via v and writes fresh
//     identity headers + ctx values from the claims.
//  3. If require=true and no validated identity emerged, returns 401.
//
// When v is nil (JWT disabled, embedded/dev mode) and require=false,
// every request passes through with empty identity ctx — useful for
// tests and the in-process embedder. When v is nil and require=true,
// every request is rejected (closed-by-default).
func RequireIdentity(v *edge.Verifier, require bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The FULL estate strip, not the four names this service happens to read.
			// A header tasksd does not write but does not delete either is one a client can
			// set and something downstream may believe — X-User-IsAdmin, X-Scope,
			// X-Billing-Account-Id and the retired names were all passing straight through.
			// The claimed org is discarded: tasksd honours no org switch, so a selection is
			// an intent with nothing to grant it.
			edge.Strip(r.Header)

			var (
				org, project, user, email string
				authed                    bool
			)
			if v != nil {
				if claims, err := v.Verify(r.Header); err == nil && claims != nil {
					// ONE write, from the one place that decides it — including the two admin
					// scopes, which this service never wrote at all and which its handlers
					// therefore could not have read even where they should.
					edge.Apply(r.Header, claims, "", nil)

					org, _ = claims.EffectiveOrg("")
					project = claims.Project
					user = claims.UserID()
					email = claims.Email
					authed = org != ""
				}
			}

			if require && !authed {
				http.Error(w, `{"error":"identity required","code":401}`, http.StatusUnauthorized)
				return
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxKeyOrgID, org)
			ctx = context.WithValue(ctx, ctxKeyProjectID, project)
			ctx = context.WithValue(ctx, ctxKeyUserID, user)
			ctx = context.WithValue(ctx, ctxKeyUserEmail, email)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// WithIdentity returns a context carrying an ALREADY-VALIDATED identity, for a
// caller that terminates the IAM trust boundary itself and embeds the Tasks HTTP
// surface in-process — e.g. the unified hanzoai/cloud binary, where the gateway
// validates the JWT and writes X-Org-Id / X-User-Id (HIP-0026) before the request
// ever reaches this handler. It is the in-process twin of RequireIdentity's write
// step: the engine reads org/user/email via OrgID/UserID/UserEmail identically,
// whether the identity was validated by the JWT path here or by a trusted
// upstream. Passing empty strings yields the unscoped (dev) context, the same as
// the no-token path — an embedder MUST therefore gate on its own validated
// principal before calling this, never on a raw client header.
func WithIdentity(ctx context.Context, org, project, user, email string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyOrgID, org)
	ctx = context.WithValue(ctx, ctxKeyProjectID, project)
	ctx = context.WithValue(ctx, ctxKeyUserID, user)
	ctx = context.WithValue(ctx, ctxKeyUserEmail, email)
	return ctx
}

// OrgID returns the org id resolved from a validated JWT, or "".
func OrgID(ctx context.Context) string { return strFromCtx(ctx, ctxKeyOrgID) }

// ProjectID returns the project id resolved from a validated JWT, or "" —
// the org/project/user identity model's middle scope. Convention: a
// project maps onto a tasks NAMESPACE inside the org's shard.
func ProjectID(ctx context.Context) string { return strFromCtx(ctx, ctxKeyProjectID) }

// UserID returns the user id resolved from a validated JWT, or "".
func UserID(ctx context.Context) string { return strFromCtx(ctx, ctxKeyUserID) }

// UserEmail returns the user email resolved from a validated JWT, or "".
func UserEmail(ctx context.Context) string { return strFromCtx(ctx, ctxKeyUserEmail) }

func strFromCtx(ctx context.Context, k ctxKey) string {
	if v, ok := ctx.Value(k).(string); ok {
		return v
	}
	return ""
}
