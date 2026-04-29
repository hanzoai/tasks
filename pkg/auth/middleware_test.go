// Copyright © 2026 Hanzo AI. MIT License.

package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gojose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// signedToken returns a JWT signed with key for use against a Validator
// configured with the matching JWKS server.
func signedToken(t *testing.T, key *rsa.PrivateKey, kid string, claims any) string {
	t.Helper()
	signer, err := gojose.NewSigner(
		gojose.SigningKey{Algorithm: gojose.RS256, Key: key},
		(&gojose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// jwksServer hosts a JWKS for the given key+kid, mirroring hanzo.id.
func jwksServer(t *testing.T, key *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	jwks := gojose.JSONWebKeySet{
		Keys: []gojose.JSONWebKey{{
			Key:       key.Public(),
			KeyID:     kid,
			Algorithm: string(gojose.RS256),
			Use:       "sig",
		}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRequireIdentity_StripsClientHeaders_RejectsWithoutToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	js := jwksServer(t, key, "kid-1")
	v := NewValidator(JWTConfig{JWKSURL: js.URL, Issuer: "https://hanzo.id", TTL: time.Minute})

	var (
		gotOrg, gotUser, gotEmail string
		called                    bool
	)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotOrg = OrgID(r.Context())
		gotUser = UserID(r.Context())
		gotEmail = UserEmail(r.Context())
		// Headers must be either empty or come from validated JWT.
		// Spoofed values from the client must NOT survive the strip.
		w.WriteHeader(http.StatusOK)
	})

	h := RequireIdentity(v, true)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/foo", nil)
	req.Header.Set(HeaderOrgID, "attacker")
	req.Header.Set(HeaderUserID, "pwned")
	req.Header.Set(HeaderUserEmail, "evil@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 with spoofed headers and require=true; got %d", rec.Code)
	}
	if called {
		t.Fatal("next called despite spoof; strip-list contract broken")
	}
	if gotOrg != "" || gotUser != "" || gotEmail != "" {
		t.Fatalf("identity ctx leaked: org=%q user=%q email=%q", gotOrg, gotUser, gotEmail)
	}
}

func TestRequireIdentity_ValidJWT_MintsHeaders(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	js := jwksServer(t, key, "kid-1")
	v := NewValidator(JWTConfig{JWKSURL: js.URL, Issuer: "https://hanzo.id", TTL: time.Minute})

	now := time.Now()
	tok := signedToken(t, key, "kid-1", IAMClaims{
		Claims: jwt.Claims{
			Issuer:   "https://hanzo.id",
			Subject:  "user-123",
			Expiry:   jwt.NewNumericDate(now.Add(time.Hour)),
			IssuedAt: jwt.NewNumericDate(now),
		},
		Owner: "hanzo",
		Email: "z@hanzo.ai",
	})

	var gotOrg, gotUser, gotEmail string
	var headerOrg, headerUser, headerEmail string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrg = OrgID(r.Context())
		gotUser = UserID(r.Context())
		gotEmail = UserEmail(r.Context())
		headerOrg = r.Header.Get(HeaderOrgID)
		headerUser = r.Header.Get(HeaderUserID)
		headerEmail = r.Header.Get(HeaderUserEmail)
		w.WriteHeader(http.StatusOK)
	})

	h := RequireIdentity(v, true)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/foo", nil)
	// Spoof headers — must be stripped before mint.
	req.Header.Set(HeaderOrgID, "attacker")
	req.Header.Set(HeaderAuthorization, "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200; got %d body=%q", rec.Code, rec.Body.String())
	}
	if gotOrg != "hanzo" || gotUser != "user-123" || gotEmail != "z@hanzo.ai" {
		t.Fatalf("ctx mismatch: org=%q user=%q email=%q", gotOrg, gotUser, gotEmail)
	}
	if headerOrg != "hanzo" || headerUser != "user-123" || headerEmail != "z@hanzo.ai" {
		t.Fatalf("headers not minted from JWT: org=%q user=%q email=%q", headerOrg, headerUser, headerEmail)
	}
}

func TestRequireIdentity_NoValidator_DevMode(t *testing.T) {
	var called bool
	var gotOrg string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotOrg = OrgID(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := RequireIdentity(nil, false)(next)
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/foo", nil)
	req.Header.Set(HeaderOrgID, "attacker")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("next must run in dev mode")
	}
	if gotOrg != "" {
		t.Fatalf("dev mode must still strip client headers; got org=%q", gotOrg)
	}
}

func TestRequireIdentity_NoValidator_RequireTrue_AlwaysRejects(t *testing.T) {
	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	h := RequireIdentity(nil, true)(next)
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/foo", nil)
	req.Header.Set(HeaderAuthorization, "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401; got %d", rec.Code)
	}
	if called {
		t.Fatal("require=true with no validator must close")
	}
}

func TestAccessorsEmptyContext(t *testing.T) {
	ctx := context.Background()
	if OrgID(ctx) != "" || UserID(ctx) != "" || UserEmail(ctx) != "" {
		t.Fatalf("expected all-empty accessors on bare context")
	}
}

func TestRequireIdentity_BadJWT_RequireTrue_Rejects(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := jwksServer(t, key, "kid-1")
	v := NewValidator(JWTConfig{JWKSURL: js.URL, Issuer: "https://hanzo.id"})

	h := RequireIdentity(v, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/foo", nil)
	req.Header.Set(HeaderAuthorization, "Bearer not-a-jwt")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for malformed JWT; got %d", rec.Code)
	}
}

func TestRequireIdentity_WrongIssuer_Rejects(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	js := jwksServer(t, key, "kid-1")
	v := NewValidator(JWTConfig{JWKSURL: js.URL, Issuer: "https://hanzo.id"})

	now := time.Now()
	tok := signedToken(t, key, "kid-1", IAMClaims{
		Claims: jwt.Claims{
			Issuer: "https://evil.example",
			Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Owner: "hanzo",
	})
	h := RequireIdentity(v, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/foo", nil)
	req.Header.Set(HeaderAuthorization, "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for wrong issuer; got %d", rec.Code)
	}
}
