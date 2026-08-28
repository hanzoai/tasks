// Copyright © 2026 Hanzo AI. MIT License.

// One request as an operation reads it, one answer as an operation writes it.
//
// Every operation on the engine's surface is a function from a call to an
// answer, holding no transport type of its own. Two renderers put an answer on
// a wire — one onto a net/http response (HTTPHandler), one onto a zip response
// (Surface) — so the same body serves both and the two cannot describe the same
// operation differently.

package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/zap-proto/zip"
)

// call is what an operation reads: the caller's identity context, the method,
// the parsed query and the raw body. The path is not here — an operation is
// reached by its route and receives the segments it needs as arguments.
type call struct {
	ctx    context.Context
	method string
	query  url.Values
	body   []byte
	// unread is the transport's failure to deliver a body. It reaches the
	// operation through decode, which is where a body that never arrived was
	// always reported.
	unread error
}

// decode parses the body as JSON into v. An empty body is not an error: many
// operations accept one and act on the zero value.
func (rq call) decode(v any) error {
	if rq.unread != nil {
		return rq.unread
	}
	if len(rq.body) == 0 {
		return nil
	}
	return json.Unmarshal(rq.body, v)
}

// stream parses the body as a JSON stream into v, so an EMPTY body reports
// io.EOF instead of leaving v at its zero value. One operation reads its body
// that way and refuses on it.
func (rq call) stream(v any) error {
	if rq.unread != nil {
		return rq.unread
	}
	return json.NewDecoder(bytes.NewReader(rq.body)).Decode(v)
}

// answer is what an operation writes: a status, at most one content type and
// the exact bytes of the body.
type answer struct {
	status  int
	ctype   string
	nosniff bool
	body    []byte
}

const (
	ctypeJSON  = "application/json"
	ctypePlain = "text/plain; charset=utf-8"
)

// data answers 200 with v as JSON, or the engine's 500 envelope when err is
// non-nil — the shape almost every read and write on this surface returns.
func data(err error, v any) answer {
	if err != nil {
		return fault(http.StatusInternalServerError, err.Error())
	}
	return answer{status: http.StatusOK, ctype: ctypeJSON, body: render(v)}
}

// fault answers code with the engine's error envelope, which carries `code` as
// a NUMBER beside the message. Clients of this surface read that shape.
func fault(code int, msg string) answer {
	return answer{status: code, ctype: ctypeJSON, body: render(map[string]any{"error": msg, "code": code})}
}

// plain answers code with msg as text, the shape http.Error writes: a trailing
// newline and the nosniff header that stops a browser guessing at the body.
func plain(code int, msg string) answer {
	return answer{status: code, ctype: ctypePlain, nosniff: true, body: []byte(msg + "\n")}
}

// absent is the answer to a path this surface routes nowhere, byte for byte
// what http.NotFound writes.
func absent() answer { return plain(http.StatusNotFound, "404 page not found") }

// empty answers code alone — no body, no content type.
func empty(code int) answer { return answer{status: code} }

// render encodes v the way json.NewEncoder(w).Encode does: HTML escaped, one
// trailing newline. A value the encoder rejects yields no bytes, which is also
// what a caller writing straight to the response would have sent.
func render(v any) []byte {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return nil
	}
	return buf.Bytes()
}

// write puts a on a net/http response.
func (a answer) write(w http.ResponseWriter) {
	if a.ctype != "" {
		w.Header().Set("Content-Type", a.ctype)
	}
	if a.nosniff {
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	w.WriteHeader(a.status)
	if len(a.body) > 0 {
		_, _ = w.Write(a.body)
	}
}

// send puts a on a zip response.
func (a answer) send(c *zip.Ctx) error {
	if a.ctype != "" {
		c.SetHeader("Content-Type", a.ctype)
	}
	if a.nosniff {
		c.SetHeader("X-Content-Type-Options", "nosniff")
	}
	return c.Bytes(a.status, a.body)
}
