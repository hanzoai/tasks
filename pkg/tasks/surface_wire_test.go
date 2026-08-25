package tasks

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zap-proto/zip"
)

// The engine hands a composer two renderings of one surface: HTTPHandler, the
// http.ServeMux it has always exposed, and Surface, the zip app a composer mounts
// with Use. A caller must not be able to tell which one answered it, so this
// drives the same requests through both and compares what comes back.
//
// It replaces a probe that printed the two answers side by side and asserted
// nothing — which passes however far the two drift, and is the shape of test
// that reports coverage it does not have.
//
// Framing is excluded and named rather than waved at: Date is the clock, and
// Content-Length is a property of how a body was written, not of what it says.
// Everything a caller reads to decide what happened — status, Content-Type,
// Location, and the body itself — is compared exactly.
func TestSurfaceAnswersWhatTheMuxAnswers(t *testing.T) {
	e, err := Embed(context.Background(), EmbedConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	defer e.Stop(context.Background())

	mux := e.HTTPHandler()
	app := zip.New(zip.Config{DisableStartupMessage: true})
	app.Use(e.Surface())

	// Each row is an address worth disagreeing about: a real read, an unknown
	// leaf, the bare noun, a deep parameterised write, an empty path segment,
	// and a path that needs cleaning.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/tasks/settings"},
		{http.MethodGet, "/v1/tasks/nope"},
		{http.MethodGet, "/v1/tasks"},
		{http.MethodPost, "/v1/tasks/namespaces/default/activities/claim"},
		{http.MethodGet, "/v1/tasks/namespaces//workflows"},
		{http.MethodGet, "/v1/tasks/namespaces/default/workflows/../x"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			want := rec.Result()
			wantBody, _ := io.ReadAll(want.Body)

			got, err := app.Test(httptest.NewRequest(tc.method, tc.path, nil))
			if err != nil {
				t.Fatalf("zip app: %v", err)
			}
			gotBody, _ := io.ReadAll(got.Body)

			if got.StatusCode != want.StatusCode {
				t.Errorf("status = %d, mux answers %d", got.StatusCode, want.StatusCode)
			}
			if string(gotBody) != string(wantBody) {
				t.Errorf("body = %q, mux answers %q", gotBody, wantBody)
			}
			for _, h := range []string{"Content-Type", "Location"} {
				if g, w := got.Header.Get(h), want.Header.Get(h); g != w {
					t.Errorf("%s = %q, mux answers %q", h, g, w)
				}
			}
		})
	}
}
