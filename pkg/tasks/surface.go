// Copyright © 2026 Hanzo AI. MIT License.

// The engine's surface as a zip application — the door a composer includes
// with app.Use, holding no net/http type anywhere in the request path.
//
// Every address here is a REAL route: zip's router matches it and zip's own
// projections read it, where an adapted http.Handler is one opaque wildcard
// over sixty-odd operations the composing router never sees.
//
// The operations behind the routes are the same functions HTTPHandler,
// ClusterHandler, MCPHandler and EventsHandler reach — a call in, an answer
// out — so the two transports cannot describe one operation differently.

package tasks

import (
	"bufio"
	"html"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/hanzoai/tasks/pkg/auth"
	"github.com/zap-proto/zip"
)

// root is where the engine answers. Every route below hangs off it.
const root = "/v1/tasks"

// Surface returns the engine's routes as a zip application:
//
//	app.Use(engine.Surface())
//
// Identity is read the way it always has been — auth.OrgID over the request's
// context — so a composer that terminates the IAM boundary states it once,
// with zip.Ctx.SetContext(auth.WithIdentity(…)), and every operation lands in
// that tenant's shard. A request carrying no identity reads the unscoped
// store, the same embedded/dev path the mux serves.
//
// The engine answers everything under /v1/tasks, including a path it routes
// nowhere: the trailing wildcard carries that refusal, and it yields to any
// route registered at a more specific address — a composer's own
// /v1/tasks/health among them.
func (e *Embedded) Surface() *zip.App {
	app := zip.New(zip.Config{})

	// The addresses that stand on their own. The MCP endpoint and the event
	// stream are NAMED here, because a route carries its address where a bare
	// handler left the choice to whoever mounted it.
	app.Get(root+"/settings", handle(e, func(rq call, _ *engine) answer { return settings(rq) }))
	app.Get(root+"/namespaces", handle(e, namespaces))
	app.Post(root+"/namespaces", handle(e, namespaces))
	app.Get(root+"/nexus", handle(e, endpoints))
	app.Get(root+"/cluster", alone(e.clusterStatus))
	app.Get(root+"/cluster/health", alone(e.clusterHealth))
	app.Post(root+"/mcp", alone(e.mcp))
	app.Get(root+"/events", e.tail)

	// One namespace, and the migration of one namespace.
	app.Get(root+"/namespaces/:ns", named(e, namespace))
	app.Delete(root+"/namespaces/:ns", named(e, namespace))
	app.Post(root+"/namespaces/:ns/migrate", func(c *zip.Ctx) error {
		ns := c.Param("ns")
		if a, ok := grammar(ns, nil); !ok {
			return a.send(c)
		}
		return e.migrate(taken(c), ns).send(c)
	})

	// Everything a namespace contains.
	for _, r := range contents {
		kind, below, _ := strings.Cut(r.path, "/")
		var rest []string
		if below != "" {
			rest = strings.Split(below, "/")
		}
		on(app, r.method, root+"/namespaces/:ns/"+r.path, func(c *zip.Ctx) error {
			rq := taken(c)
			ns := c.Param("ns")
			sub := segments(c, rest)
			if a, ok := grammar(ns, sub); !ok {
				return a.send(c)
			}
			return resource(rq, e.engine.As(Org(auth.OrgID(rq.ctx))), ns, kind, sub).send(c)
		})
	}

	// A path under /v1/tasks that no route above claims. The engine has always
	// answered it with net/http's plain-text refusal rather than a status of
	// its own, and clients read that shape.
	//
	// Before refusing, it answers the redirect a path needing cleaning earns.
	// http.ServeMux tidies a path — an empty segment, a dot segment — and
	// redirects to the tidied form before it matches anything, so a request
	// like /namespaces//workflows reaches a route there and 404s here. That is
	// a difference a caller can see, so it is reproduced rather than accepted.
	app.All(root+"/*", func(c *zip.Ctx) error {
		if to, dirty := tidy(c.Path()); dirty {
			if q := string(c.Fiber().RequestCtx().URI().QueryString()); q != "" {
				to += "?" + q
			}
			c.SetHeader("Location", to)
			// http.Redirect writes the hypertext body for a GET or a HEAD and
			// for no other method.
			switch c.Method() {
			case http.MethodGet, http.MethodHead:
				return answer{
					status: http.StatusTemporaryRedirect,
					ctype:  "text/html; charset=utf-8",
					body:   []byte("<a href=\"" + html.EscapeString(to) + "\">Temporary Redirect</a>.\n\n"),
				}.send(c)
			default:
				return empty(http.StatusTemporaryRedirect).send(c)
			}
		}
		return absent().send(c)
	})

	return app
}

// row is one address a namespace's contents answer: the method, and the path
// below /v1/tasks/namespaces/:ns/. The first segment names the resource; a
// ":name" segment is a path parameter and every other segment reaches the
// operation as itself — which is what the mux hands it after a split.
type row struct {
	method string
	path   string
}

const (
	get  = http.MethodGet
	post = http.MethodPost
	del  = http.MethodDelete
)

// contents is the engine's route table under one namespace. Sixty-one
// addresses, each registered on its own with its own method, which is the
// whole difference between this and a wildcard.
var contents = []row{
	{get, "workflows"},
	{post, "workflows"},
	{post, "workflows/signal-with-start"},
	{get, "workflows/:wf"},
	{post, "workflows/:wf/cancel"},
	{post, "workflows/:wf/terminate"},
	{post, "workflows/:wf/signal"},
	{get, "workflows/:wf/history"},
	{post, "workflows/:wf/query"},
	{post, "workflows/:wf/metadata"},
	{get, "workflows/:wf/executions"},
	{post, "workflows/:wf/reset"},

	{get, "schedules"},
	{post, "schedules"},
	{get, "schedules/:id"},
	{post, "schedules/:id"},
	{del, "schedules/:id"},
	{post, "schedules/:id/trigger"},
	{get, "schedules/:id/matching-times"},
	{post, "schedules/:id/pause"},
	{post, "schedules/:id/unpause"},

	{get, "batches"},
	{post, "batches"},
	{get, "batches/:id"},
	{post, "batches/:id/terminate"},

	{get, "deployments"},
	{post, "deployments"},
	{get, "deployments/:id"},
	{post, "deployments/:id"},
	{del, "deployments/:id"},
	{post, "deployments/:id/set-current"},
	{post, "deployments/:id/versions"},
	{post, "deployments/:id/versions/:build"},
	{del, "deployments/:id/versions/:build"},
	{post, "deployments/:id/versions/:build/validate"},

	{get, "nexus"},
	{post, "nexus"},
	{del, "nexus/:id"},

	{get, "identities"},
	{post, "identities"},
	{del, "identities/:id"},

	{get, "task-queues"},
	{get, "task-queues/:queue"},
	{get, "task-queues/:queue/workers"},
	{get, "task-queues/:queue/partitions"},

	{get, "workers"},
	{get, "workers/:id"},

	{get, "search-attributes"},
	{post, "search-attributes"},
	{del, "search-attributes/:attr"},

	{post, "metadata"},

	{get, "archival"},

	{get, "activities"},
	{post, "activities"},
	{post, "activities/claim"},
	{get, "activities/:id/:run"},
	{post, "activities/:id/:run/cancel"},
	{post, "activities/:id/:run/complete"},
	{post, "activities/:id/:run/fail"},
	{post, "activities/:id/:run/heartbeat"},
	{get, "activities/:id/:run/history"},
}

// on registers h at path for one method. A method outside the three the engine
// serves is a mistake in the table, and panicking at composition is where it
// can still be seen — a silently unregistered route answers 404 in production.
func on(app *zip.App, method, path string, h zip.Handler) {
	switch method {
	case get:
		app.Get(path, h)
	case post:
		app.Post(path, h)
	case del:
		app.Delete(path, h)
	default:
		panic("tasks: no route method " + method)
	}
}

// segments resolves a row's path below the resource name into the values the
// operation reads: a parameter takes the segment that matched it, anything
// else is itself.
func segments(c *zip.Ctx, rest []string) []string {
	out := make([]string, len(rest))
	for i, s := range rest {
		if name, ok := strings.CutPrefix(s, ":"); ok {
			out[i] = c.Param(name)
			continue
		}
		out[i] = s
	}
	return out
}

// handle renders one operation that reads the org-scoped engine.
func handle(e *Embedded, fn func(rq call, en *engine) answer) zip.Handler {
	return func(c *zip.Ctx) error {
		rq := taken(c)
		return fn(rq, e.engine.As(Org(auth.OrgID(rq.ctx)))).send(c)
	}
}

// alone renders one operation that resolves whatever engine view it needs.
func alone(fn func(rq call) answer) zip.Handler {
	return func(c *zip.Ctx) error { return fn(taken(c)).send(c) }
}

// named renders one operation on a namespace, after that namespace has
// satisfied the grammar a constructed store key depends on.
func named(e *Embedded, fn func(rq call, en *engine, ns string) answer) zip.Handler {
	return func(c *zip.Ctx) error {
		rq := taken(c)
		ns := c.Param("ns")
		if a, ok := grammar(ns, nil); !ok {
			return a.send(c)
		}
		return fn(rq, e.engine.As(Org(auth.OrgID(rq.ctx))), ns).send(c)
	}
}

// tail streams the engine's event feed. It ends when the client stops reading
// — a flush onto a closed connection reports it — which is how a feed with no
// natural end unsubscribes.
func (e *Embedded) tail(c *zip.Ctx) error {
	for _, h := range eventHeaders {
		c.SetHeader(h[0], h[1])
	}
	ctx := c.Context()
	org := auth.OrgID(ctx)
	return c.SendStreamWriter(func(w *bufio.Writer) { e.feed(ctx, org, w, w.Flush) })
}

// taken reads one zip request as an operation reads it. The query is parsed
// from the raw string by the same parser net/http uses, so a repeated or
// malformed parameter resolves identically on both transports.
func taken(c *zip.Ctx) call {
	q, _ := url.ParseQuery(string(c.Fiber().Request().URI().QueryString()))
	return call{ctx: c.Context(), method: c.Method(), query: q, body: c.Body()}
}

// tidy is the path http.ServeMux would route a request at, and whether that
// differs from the one it arrived on. It is net/http's own cleanPath: collapse
// the path, then put back a trailing slash the collapse removed, because
// "/x/" and "/x" are two addresses to a mux and only the first names a subtree.
func tidy(p string) (string, bool) {
	if p == "" {
		return "/", true
	}
	q := p
	if q[0] != '/' {
		q = "/" + q
	}
	c := path.Clean(q)
	if q[len(q)-1] == '/' && c != "/" {
		if len(q) == len(c)+1 && strings.HasPrefix(q, c) {
			c = q
		} else {
			c += "/"
		}
	}
	return c, c != p
}
