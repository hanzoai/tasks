// Copyright © 2026 Hanzo AI. MIT License.

// Package auth — JWT validation against IAM JWKS. Used by RequireIdentity
// middleware to mint identity headers from validated tokens. Mirrors the
// pattern in hanzoai/gateway: TTL-cached JWKS, fail-stale, exact issuer
// and audience checks.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	gojose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// IAMClaims is the subset of Casdoor/hanzo.id claims tasksd cares about.
// `owner` is the org slug (X-Org-Id), `sub` is the user id (X-User-Id),
// `email` is the user email (X-User-Email).
type IAMClaims struct {
	jwt.Claims

	Owner             string `json:"owner"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
}

// JWTConfig configures the validator. Zero values disable JWT validation
// (auth.RequireIdentity falls back to header-pass-through, gated by the
// require flag).
type JWTConfig struct {
	JWKSURL  string        // e.g. https://hanzo.id/.well-known/jwks
	Issuer   string        // e.g. https://hanzo.id
	Audience string        // optional; "" → audience check skipped
	TTL      time.Duration // JWKS cache TTL; 0 → 5 min
}

// Validator verifies bearer tokens against JWKS and returns claims.
type Validator struct {
	cfg   JWTConfig
	cache *jwksCache
}

// NewValidator returns nil when cfg is empty (JWT disabled).
func NewValidator(cfg JWTConfig) *Validator {
	if cfg.JWKSURL == "" {
		return nil
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	return &Validator{
		cfg: cfg,
		cache: &jwksCache{
			url:    cfg.JWKSURL,
			ttl:    cfg.TTL,
			client: &http.Client{Timeout: 10 * time.Second},
		},
	}
}

// Validate parses and verifies the bearer token. Returns the IAM claims
// on success, or an error describing the failure mode.
func (v *Validator) Validate(ctx context.Context, bearer string) (*IAMClaims, error) {
	if v == nil {
		return nil, errors.New("jwt: validator not configured")
	}
	bearer = strings.TrimSpace(bearer)
	bearer = strings.TrimPrefix(bearer, "Bearer ")
	bearer = strings.TrimPrefix(bearer, "bearer ")
	if bearer == "" {
		return nil, errors.New("jwt: empty token")
	}

	tok, err := jwt.ParseSigned(bearer, []gojose.SignatureAlgorithm{gojose.RS256, gojose.RS384, gojose.RS512, gojose.ES256, gojose.ES384, gojose.ES512})
	if err != nil {
		return nil, fmt.Errorf("jwt: parse: %w", err)
	}

	keys, err := v.cache.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("jwt: jwks: %w", err)
	}

	var claims IAMClaims
	var lastErr error
	verified := false
	for _, k := range keys.Keys {
		if err := tok.Claims(k.Key, &claims); err == nil {
			verified = true
			break
		} else {
			lastErr = err
		}
	}
	if !verified {
		if lastErr == nil {
			lastErr = errors.New("no key matched")
		}
		return nil, fmt.Errorf("jwt: verify: %w", lastErr)
	}

	exp := jwt.Expected{Time: time.Now()}
	if v.cfg.Issuer != "" {
		exp.Issuer = v.cfg.Issuer
	}
	if v.cfg.Audience != "" {
		exp.AnyAudience = jwt.Audience{v.cfg.Audience}
	}
	if err := claims.Claims.Validate(exp); err != nil {
		return nil, fmt.Errorf("jwt: validate: %w", err)
	}
	return &claims, nil
}

// jwksCache is a TTL-bounded cache for JWKS keys. Stale-on-error: if the
// cache has any keys and a refresh fails, the stale keys are returned —
// brief IAM blips don't take tasksd down.
type jwksCache struct {
	mu        sync.RWMutex
	keys      *gojose.JSONWebKeySet
	fetchedAt time.Time
	ttl       time.Duration
	url       string
	client    *http.Client
}

func (c *jwksCache) get(ctx context.Context) (*gojose.JSONWebKeySet, error) {
	c.mu.RLock()
	if c.keys != nil && time.Since(c.fetchedAt) < c.ttl {
		k := c.keys
		c.mu.RUnlock()
		return k, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys != nil && time.Since(c.fetchedAt) < c.ttl {
		return c.keys, nil
	}

	keys, err := c.fetch(ctx)
	if err != nil {
		if c.keys != nil {
			return c.keys, nil
		}
		return nil, err
	}
	c.keys = keys
	c.fetchedAt = time.Now()
	return keys, nil
}

func (c *jwksCache) fetch(ctx context.Context) (*gojose.JSONWebKeySet, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var ks gojose.JSONWebKeySet
	if err := json.Unmarshal(body, &ks); err != nil {
		return nil, err
	}
	return &ks, nil
}
