// Copyright © 2026 Hanzo AI. MIT License.

package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/base64"
	"math/big"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hanzoai/authz"
)

// signedToken returns a JWT signed with key, naming it by kid.
//
// It signs with golang-jwt — the library IAM SIGNS with and hanzoai/authz verifies
// with. Minting with a different library than the one under test is how a test comes
// to certify a reader nobody runs.
func signedToken(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// iamClaims is the shape IAM actually signs for a person: the home-org membership
// set is present, which is what marks the principal HUMAN rather than a machine.
func iamClaims(owner, sub, email, project string, exp time.Time) *authz.Claims {
	c := &authz.Claims{
		Owner: owner, Email: email, Project: project,
		PreferredUsername: sub,
		Orgs:              []authz.Membership{{Org: owner, Role: authz.Member}},
	}
	c.Issuer = testIssuer
	c.Subject = sub
	c.ExpiresAt = jwt.NewNumericDate(exp)
	c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	return c
}

const testIssuer = "https://hanzo.id"

// jwksServer hosts a JWKS for the given key+kid, mirroring hanzo.id.
func jwksServer(t *testing.T, key *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	jwks := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}}}
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

	tok := signedToken(t, key, "kid-1",
		iamClaims("hanzo", "user-123", "z@hanzo.ai", "acme-app", time.Now().Add(time.Hour)))

	var gotOrg, gotProject, gotUser, gotEmail string
	var headerOrg, headerProject, headerUser, headerEmail string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrg = OrgID(r.Context())
		gotProject = ProjectID(r.Context())
		gotUser = UserID(r.Context())
		gotEmail = UserEmail(r.Context())
		headerOrg = r.Header.Get(HeaderOrgID)
		headerProject = r.Header.Get(HeaderProjectID)
		headerUser = r.Header.Get(HeaderUserID)
		headerEmail = r.Header.Get(HeaderUserEmail)
		w.WriteHeader(http.StatusOK)
	})

	h := RequireIdentity(v, true)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/foo", nil)
	// Spoof headers — must be stripped before mint.
	req.Header.Set(HeaderOrgID, "attacker")
	req.Header.Set(HeaderProjectID, "attacker-project")
	req.Header.Set(HeaderAuthorization, "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200; got %d body=%q", rec.Code, rec.Body.String())
	}
	if gotOrg != "hanzo" || gotProject != "acme-app" || gotUser != "user-123" || gotEmail != "z@hanzo.ai" {
		t.Fatalf("ctx mismatch: org=%q project=%q user=%q email=%q", gotOrg, gotProject, gotUser, gotEmail)
	}
	if headerOrg != "hanzo" || headerProject != "acme-app" || headerUser != "user-123" || headerEmail != "z@hanzo.ai" {
		t.Fatalf("headers not minted from JWT: org=%q project=%q user=%q email=%q", headerOrg, headerProject, headerUser, headerEmail)
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

	foreign := iamClaims("hanzo", "user-123", "", "", time.Now().Add(time.Hour))
	foreign.Issuer = "https://evil.example"
	tok := signedToken(t, key, "kid-1", foreign)
	h := RequireIdentity(v, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/foo", nil)
	req.Header.Set(HeaderAuthorization, "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for wrong issuer; got %d", rec.Code)
	}
}

// tasksd strips the WHOLE estate identity set, not just the four names it reads.
//
// It used to strip exactly X-Org-Id / X-Project-Id / X-User-Id / X-User-Email, so
// every other minted name — X-User-IsAdmin and X-User-Permissions (the platform and
// money signals), X-User-Owner, X-Billing-Account-Id, X-Scope, X-Workspace-Id, and
// the retired names — passed through from the client untouched. tasksd terminates
// identity itself (its ingress routes straight to the Service), so nothing upstream
// was deleting them either.
func TestRequireIdentity_StripsTheWholeEstateSet(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	js := jwksServer(t, key, "kid-1")
	v := NewValidator(JWTConfig{JWKSURL: js.URL, Issuer: testIssuer, TTL: time.Minute})

	seen := map[string]string{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range append(append([]string{}, authz.Headers...), authz.Retired...) {
			seen[h] = r.Header.Get(h)
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/foo", nil)
	for _, h := range append(append([]string{}, authz.Headers...), authz.Retired...) {
		req.Header.Set(h, "forged")
	}
	// An ordinary member: entitled to none of the authority headers.
	req.Header.Set(HeaderAuthorization, "Bearer "+signedToken(t, key, "kid-1",
		iamClaims("hanzo", "user-123", "z@hanzo.ai", "", time.Now().Add(time.Hour))))

	rec := httptest.NewRecorder()
	RequireIdentity(v, true)(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token refused with %d", rec.Code)
	}

	for _, h := range []string{
		authz.HeaderUserAdmin, authz.HeaderUserOrgAdmin, authz.HeaderUserPermissions,
		authz.HeaderScope, authz.HeaderScopeRole, authz.HeaderWorkspace,
		authz.HeaderBillingAccount,
	} {
		if seen[h] != "" {
			t.Errorf("forged %s survived as %q", h, seen[h])
		}
	}
	for _, h := range authz.Retired {
		if seen[h] != "" {
			t.Errorf("retired %s survived as %q", h, seen[h])
		}
	}
	// What the token DID earn is re-minted, so the strip is not just deletion.
	if seen[authz.HeaderOrg] != "hanzo" {
		t.Errorf("%s = %q, want hanzo", authz.HeaderOrg, seen[authz.HeaderOrg])
	}
	if seen[authz.HeaderUserOwner] != "hanzo" {
		t.Errorf("%s = %q, want hanzo", authz.HeaderUserOwner, seen[authz.HeaderUserOwner])
	}
}

// An admin-org MACHINE reaches tasksd with no authority. IAM's client_credentials
// grant signs no membership set, which is the machine signal; before this, tasksd
// had no machine predicate at all and would have minted its `owner` as the org while
// letting a forged X-User-IsAdmin ride through untouched.
func TestRequireIdentity_AdminOrgMachineCarriesNoAuthority(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	js := jwksServer(t, key, "kid-1")
	v := NewValidator(JWTConfig{JWKSURL: js.URL, Issuer: testIssuer, TTL: time.Minute})

	machine := iamClaims(authz.AdminOrg, authz.AdminOrg+"/kms-sync", "", "", time.Now().Add(time.Hour))
	machine.Orgs = nil // no membership set: IAM's client_credentials shape
	machine.IsAdmin = true

	seen := map[string]string{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[authz.HeaderUserAdmin] = r.Header.Get(authz.HeaderUserAdmin)
		seen[authz.HeaderUserOrgAdmin] = r.Header.Get(authz.HeaderUserOrgAdmin)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/foo", nil)
	req.Header.Set(authz.HeaderUserAdmin, "true") // forged, on top of the real token
	req.Header.Set(HeaderAuthorization, "Bearer "+signedToken(t, key, "kid-1", machine))

	rec := httptest.NewRecorder()
	RequireIdentity(v, true)(next).ServeHTTP(rec, req)

	if seen[authz.HeaderUserAdmin] != "" {
		t.Errorf("an admin-org machine reached the handler with %s=%q",
			authz.HeaderUserAdmin, seen[authz.HeaderUserAdmin])
	}
	if seen[authz.HeaderUserOrgAdmin] != "" {
		t.Errorf("an admin-org machine reached the handler with %s=%q",
			authz.HeaderUserOrgAdmin, seen[authz.HeaderUserOrgAdmin])
	}
}

// An unset TASKSD_WHITELABEL_ISSUERS splits into one EMPTY string, and an empty
// issuer must not become a trusted one — a token carrying no `iss` would match it.
// The verifier is built from the same code path production uses.
func TestIssuerAllowlistDropsEmptyEntries(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	js := jwksServer(t, key, "kid-1")
	v := NewValidator(JWTConfig{
		JWKSURL: js.URL,
		Issuer:  testIssuer,
		Issuers: strings.Split("", ","), // exactly what an unset env var yields
		TTL:     time.Minute,
	})

	// The real issuer still works.
	good := signedToken(t, key, "kid-1", iamClaims("hanzo", "u", "", "", time.Now().Add(time.Hour)))
	if _, err := v.VerifyRaw(good); err != nil {
		t.Fatalf("the primary issuer was refused: %v", err)
	}

	// A token carrying NO issuer must not be admitted by the empty entry.
	none := iamClaims("hanzo", "u", "", "", time.Now().Add(time.Hour))
	none.Issuer = ""
	if _, err := v.VerifyRaw(signedToken(t, key, "kid-1", none)); err == nil {
		t.Error("a token with no issuer was trusted")
	}
}

// A brand this tasksd fronts is admitted by configuration.
func TestWhiteLabelIssuerIsAdmitted(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	js := jwksServer(t, key, "kid-1")
	v := NewValidator(JWTConfig{
		JWKSURL: js.URL, Issuer: testIssuer,
		Issuers: []string{"https://lux.id", "https://zoo.id"},
		TTL:     time.Minute,
	})

	for _, iss := range []string{testIssuer, "https://lux.id", "https://zoo.id"} {
		c := iamClaims("acme", "u", "", "", time.Now().Add(time.Hour))
		c.Issuer = iss
		if _, err := v.VerifyRaw(signedToken(t, key, "kid-1", c)); err != nil {
			t.Errorf("brand issuer %s was refused: %v", iss, err)
		}
	}
	rogue := iamClaims("acme", "u", "", "", time.Now().Add(time.Hour))
	rogue.Issuer = "https://evil.test"
	if _, err := v.VerifyRaw(signedToken(t, key, "kid-1", rogue)); err == nil {
		t.Error("an untrusted issuer was admitted")
	}
}
