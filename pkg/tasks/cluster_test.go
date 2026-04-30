// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCluster_StatusAndHealth(t *testing.T) {
	emb, err := Embed(context.Background(), EmbedConfig{ZAPPort: 0, NodeID: "n1"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(context.Background())

	srv := httptest.NewServer(emb.ClusterHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/tasks/cluster")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d %s", resp.StatusCode, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["replicator"] != "local" {
		t.Fatalf("replicator = %v, want local", got["replicator"])
	}
	if _, ok := got["validators"]; !ok {
		t.Fatal("missing validators field")
	}

	resp2, err := http.Get(srv.URL + "/v1/tasks/cluster/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("health: %d", resp2.StatusCode)
	}
}

func TestCluster_MigrateEndpoint(t *testing.T) {
	emb, err := Embed(context.Background(), EmbedConfig{ZAPPort: 0, NodeID: "n1"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(context.Background())

	// Pre-touch the namespace so a shard exists.
	_ = emb.engine.RegisterNamespace(Namespace{
		NamespaceInfo: NamespaceInfo{Name: "scratch", State: "NAMESPACE_STATE_REGISTERED"},
	})

	srv := httptest.NewServer(emb.ClusterHandler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/tasks/namespaces/scratch/migrate",
		"application/json",
		strings.NewReader(`{"toNode":"n2"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("migrate: %d %s", resp.StatusCode, body)
	}
}
