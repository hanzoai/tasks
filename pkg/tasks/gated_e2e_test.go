// Copyright © 2026 Hanzo AI. MIT License.

package tasks_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"encoding/base64"
	"math/big"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hanzoai/authz"

	"github.com/hanzoai/tasks/pkg/auth"
	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/worker"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
	"github.com/hanzoai/tasks/pkg/tasks"
	luxlog "github.com/luxfi/log"
)

// gatedGreet / gatedHello — a trivial durable workflow used to prove real durable
// execution runs over the identity-gated ServeGated listener (not just a
// control-plane round-trip): the workflow body schedules an activity and returns
// its result, exactly the flow the consumer cutover depends on.
func gatedGreet(ctx context.Context, who string) (string, error) { return "hi, " + who, nil }

func gatedHello(ctx workflow.Context, name string) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 5 * time.Second})
	var out string
	if err := workflow.ExecuteActivity(ctx, gatedGreet, name).Get(ctx, &out); err != nil {
		return "", err
	}
	return out, nil
}

// gatedJWKS hosts a JWKS for key+kid, mirroring hanzo.id's signing surface.
func gatedJWKS(t *testing.T, key *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	set := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// gatedSign issues a JWT for a PERSON in owner, the shape hanzo.id mints: the
// home-org membership set is present, which is what marks the principal human. It
// signs with golang-jwt, the library IAM signs with and hanzoai/authz verifies with.
func gatedSign(t *testing.T, key *rsa.PrivateKey, kid, issuer, owner string) string {
	t.Helper()
	claims := &authz.Claims{
		Owner:             owner,
		PreferredUsername: "u1",
		Orgs:              []authz.Membership{{Org: owner, Role: authz.Member}},
	}
	claims.Issuer = issuer
	claims.Subject = "u1"
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestE2E_ServeGated_DurableWorkflow_RequiresIdentity proves the cutover property:
// the embedded engine, exposed on a SECOND gated ZAP listener, refuses durable
// execution to an unauthenticated caller (401) and runs a real durable workflow
// to COMPLETED for a token-bearing caller, org-scoped to the token owner.
func TestE2E_ServeGated_DurableWorkflow_RequiresIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	js := gatedJWKS(t, key, "kid-1")
	validator := auth.NewValidator(auth.JWTConfig{JWKSURL: js.URL, Issuer: "https://hanzo.id", TTL: time.Minute})

	emb, err := tasks.Embed(ctx, tasks.EmbedConfig{Address: fmt.Sprintf(":%d", freePort(t))})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(ctx)

	// ServeGated refuses to expose the engine without a validator.
	if err := emb.ServeGated(ctx, fmt.Sprintf(":%d", freePort(t)), nil); err == nil {
		t.Fatal("ServeGated accepted a nil validator; want refusal")
	}

	gatedPort := freePort(t)
	if err := emb.ServeGated(ctx, fmt.Sprintf(":%d", gatedPort), validator); err != nil {
		t.Fatalf("serve gated: %v", err)
	}
	gatedAddr := fmt.Sprintf("127.0.0.1:%d", gatedPort)

	// A caller WITHOUT a token is refused durable execution on the gated listener.
	anon, err := client.Dial(client.Options{Address: gatedAddr, Namespace: "default", DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("dial anon: %v", err)
	}
	if _, err := anon.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: "default"}, "GatedHello", "world"); err == nil {
		t.Fatal("gated listener accepted an unauthenticated ExecuteWorkflow; want 401")
	}
	anon.Close()

	// A caller WITH a valid token runs a real durable workflow, org-scoped to owner.
	tok := gatedSign(t, key, "kid-1", "https://hanzo.id", "acme")
	cli, err := client.Dial(client.Options{
		Address:     gatedAddr,
		Namespace:   "default",
		DialTimeout: 5 * time.Second,
		Token:       client.StaticToken(tok),
	})
	if err != nil {
		t.Fatalf("dial authed: %v", err)
	}
	defer cli.Close()

	// Register the org-scoped namespace the authed caller will execute in.
	// (Under WithOrg(owner) the store is org-prefixed, so the root default
	// namespace is invisible; ExecuteWorkflow 400s until the org registers it.)
	if err := cli.RegisterNamespace(ctx, &client.RegisterNamespaceRequest{Name: "default"}); err != nil {
		t.Fatalf("register namespace: %v", err)
	}

	w := worker.New(cli, "default", worker.Options{
		MaxConcurrentWorkflowTaskPollers:       1,
		MaxConcurrentActivityExecutionSize:     1,
		MaxConcurrentWorkflowTaskExecutionSize: 1,
		Logger:                                 luxlog.New(),
	})
	w.RegisterWorkflowWithOptions(gatedHello, worker.RegisterWorkflowOptions{Name: "GatedHello"})
	w.RegisterActivity(gatedGreet)
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer w.Stop()
	time.Sleep(150 * time.Millisecond)

	run, err := cli.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "gated-hello-" + time.Now().UTC().Format("150405.000000"),
		TaskQueue: "default",
	}, "GatedHello", "world")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	var last client.WorkflowStatus
	for time.Now().Before(deadline) {
		info, derr := cli.DescribeWorkflow(ctx, run.GetID(), run.GetRunID())
		if derr != nil {
			t.Fatalf("describe: %v", derr)
		}
		last = info.Status
		if info.Status == client.WorkflowStatusCompleted {
			break
		}
		if info.Status == client.WorkflowStatusFailed ||
			info.Status == client.WorkflowStatusTimedOut ||
			info.Status == client.WorkflowStatusTerminated {
			t.Fatalf("gated workflow ended in non-success terminal state: %d (info=%+v)", info.Status, info)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if last != client.WorkflowStatusCompleted {
		t.Fatalf("gated workflow did not complete; last status=%d", last)
	}
}
