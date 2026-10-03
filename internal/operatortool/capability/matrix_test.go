package capability

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

// These regressions catch unreviewed dashboard additions, stale/duplicate decisions,
// hidden form action variants, and premature parent parity claims.
func TestDashboardCapabilityCoverage(t *testing.T) {
	matrix, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := Discover(os.DirFS("../../.."))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(matrix, candidates, true); err != nil {
		t.Fatal(err)
	}
	for _, op := range matrix.Operations {
		if op.Status != "implemented" || op.Audience != "operator" {
			continue
		}
		definition, ok := operatortool.Lookup(op.Tool.Name)
		if !ok {
			t.Errorf("%s claims an implemented tool absent from the registry: %s", op.ID, op.Tool.Name)
			continue
		}
		if op.Tool.Set != definition.Meta.Toolset {
			t.Errorf("%s records toolset %s; %s advertises %s", op.ID, op.Tool.Set, definition.Name, definition.Meta.Toolset)
		}
		if json.Valid([]byte(op.Tool.Arguments)) {
			var recorded, advertised any
			if err := json.Unmarshal([]byte(op.Tool.Arguments), &recorded); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(definition.InputSchema, &advertised); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(recorded, advertised) {
				t.Errorf("%s implemented schema differs from %s", op.ID, definition.Name)
			}
		}
	}
	t.Logf("%d source sites, %d operation decisions", len(candidates), len(matrix.Operations))
}

func TestCapabilityDrift(t *testing.T) {
	original := fstest.MapFS{
		"internal/web/server.go":      {Data: []byte(`package web; import "github.com/labstack/echo/v4"; const routePath="/existing"; func routes(router *echo.Echo) { router.GET(routePath, existing) }`)},
		"web/conversation/src/api.ts": {Data: []byte(`export const existing = () => send(Result, "GET", "/existing");`)},
	}
	base, err := Discover(original)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	op := fixture.Operations[slices.IndexFunc(fixture.Operations, func(o Operation) bool { return o.Audience == "operator" })]
	op.ID = "synthetic.existing"
	op.Sources = base
	op.Status = "pending"
	matrix := Matrix{Schema: 1, Parent: fixture.Parent, Operations: []Operation{op}}
	if err := Validate(matrix, base, false); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, path, source string
		change             func(*Matrix)
		want               string
		parity             bool
		replacement        bool
	}{
		{name: "new route on named Echo receiver", path: "internal/web/extra.go", source: `package web; import "github.com/labstack/echo/v4"; func extra(dashboard *echo.Echo) { dashboard.POST("/new", create) }`, want: "uncovered source"},
		{name: "new grouped route", path: "internal/hubserver/extra.go", source: `package hubserver; import "github.com/labstack/echo/v4"; func extra(e *echo.Echo) { group := e.Group("/org"); group.DELETE("/new", remove) }`, want: "uncovered source"},
		{name: "changed route constant", path: "internal/web/server.go", source: `package web; import "github.com/labstack/echo/v4"; const routePath="/new"; func routes(router *echo.Echo) { router.GET(routePath, existing) }`, want: "uncovered source"},
		{name: "new hub route", path: "internal/hubserver/extra.go", source: `package hubserver; import "github.com/labstack/echo/v4"; func extra(e *echo.Echo) { e.POST(nativeBase+"/new", create) }`, want: "uncovered source"},
		{name: "new shared entry route", path: "internal/cloudentry/extra.go", source: `package cloudentry; import "github.com/labstack/echo/v4"; func extra(e *echo.Echo) { e.GET("/new", read) }`, want: "uncovered source"},
		{name: "browser-only form", path: "internal/web/templates/new.templ", source: `templ New() { <form action="/new" method="post"><button>Go</button></form> }`, want: "uncovered source"},
		{name: "hidden action variant", path: "internal/web/templates/new.templ", source: `<input type="hidden" name="action" value="destroy">`, want: "uncovered source"},
		{name: "new htmx action", path: "internal/web/templates/new.templ", source: `<button hx-delete={ destroyURL(item) }>Go</button>`, want: "uncovered source"},
		{name: "htmx action after arrow attribute", path: "internal/web/templates/new.templ", source: `<button onClick={() => toggle()} hx-delete="/new">Go</button>`, want: "uncovered source"},
		{name: "new React submit", path: "web/conversation/src/new.tsx", source: `<form onSubmit={createNew}><button>Go</button></form>`, want: "uncovered source"},
		{name: "new frontend request", path: "web/conversation/src/new.ts", source: "const add = () => fetch(`/new/${id}`, {method:'POST'});", want: "uncovered source"},
		{name: "new browser route", path: "web/conversation/src/new.tsx", source: `<Route path="/new" element={<New />} />`, want: "uncovered source"},
		{name: "request action argument", path: "web/conversation/src/api.ts", source: `export const existing = () => send(Result, "GET", "/existing", {action:"destroy"});`, want: "uncovered source"},
		{name: "current selector and attachment replacements", path: "web/conversation/src/api.ts", source: `
const list = () => send(ConversationListResponse, "GET", url("/conversations", {subject_work_item_id: input.subjectWorkItemId, cursor: input.cursor}));
const create = () => send(CreateConversationResponse, "POST", "/conversations", {key: input.key, ...(input.subjectWorkItemId === undefined ? {} : {subject_work_item_id: input.subjectWorkItemId})});
const work = () => send(WorkItemPage, "GET", url("/work-items", {q: input.q?.trim() || undefined, include: input.includeWork === true ? "work" : undefined}));
const attempts = () => send(AttemptPage, "GET", url("/attempts", {limit, cursor}), undefined, signal);
const workspace = new EventSource(http.eventsUrl(projectId, workspaceId), {withCredentials: true});
const upload = () => send(WorkAttachment, "POST", project(projectId) + "/attachments", body);
`, want: "uncovered source", replacement: true},
		{name: "duplicate operation", change: func(m *Matrix) { m.Operations = append(m.Operations, m.Operations[0]) }, want: "duplicate or empty operation ID"},
		{name: "duplicate site owner", change: func(m *Matrix) {
			other := m.Operations[0]
			other.ID = "another"
			m.Operations = append(m.Operations, other)
		}, want: "duplicate source ownership"},
		{name: "orphan operation", change: func(m *Matrix) { m.Operations[0].Sources = nil }, want: "orphan operation"},
		{name: "stale source", path: "web/conversation/src/api.ts", source: `export const existing = () => {};`, want: "orphan source"},
		{name: "operator exclusion", change: func(m *Matrix) { m.Operations[0].Status = "excluded" }, want: "operator operation excluded"},
		{name: "future exclusion", change: func(m *Matrix) {
			m.Operations[0].Audience = "transport"
			m.Operations[0].Status = "excluded"
			m.Operations[0].Decision = "all future endpoints"
			m.Operations[0].Tool = Tool{}
		}, want: "future-proof exclusion"},
		{name: "parent parity incomplete", parity: true, want: "unimplemented operator operation"},
		{name: "parent parity complete", parity: true, change: func(m *Matrix) { m.Operations[0].Status = "implemented" }},
		{name: "unassigned child", change: func(m *Matrix) { m.Operations[0].Owner = "digitaldrywood/detent#3259" }, want: "invalid child owner"},
		{name: "mismatched source metadata", change: func(m *Matrix) { m.Operations[0].Sources[0].Method = "DELETE" }, want: "source metadata mismatch"},
		{name: "duplicate availability", change: func(m *Matrix) {
			m.Operations[0].Availability = append(m.Operations[0].Availability, m.Operations[0].Availability[0])
		}, want: "duplicate availability"},
		{name: "unknown tracker", change: func(m *Matrix) { m.Operations[0].Availability[0].Trackers = []string{"future_tracker"} }, want: "unknown tracker"},
		{name: "missing tool annotations", change: func(m *Matrix) { m.Operations[0].Tool.Annotations = nil }, want: "missing typed tool proposal"},
		{name: "staff tool leakage", change: func(m *Matrix) { m.Operations[0].Audience = "staff"; m.Operations[0].Status = "excluded" }, want: "excluded site exposes operator tool"},
		{name: "unknown deployment", change: func(m *Matrix) { m.Operations[0].Availability[0].Deployment = "any_future_deployment" }, want: "invalid deployment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := fstest.MapFS{}
			for path, file := range original {
				root[path] = file
			}
			if tt.path != "" {
				root[tt.path] = &fstest.MapFile{Data: []byte(tt.source)}
			}
			actual, err := Discover(root)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(matrix)
			if err != nil {
				t.Fatal(err)
			}
			var current Matrix
			if err := json.Unmarshal(data, &current); err != nil {
				t.Fatal(err)
			}
			if tt.change != nil {
				tt.change(&current)
			}
			err = Validate(current, actual, tt.parity)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want %q, got %v", tt.want, err)
			}
			if tt.replacement {
				if len(actual) != 7 || strings.Count(err.Error(), "uncovered source ") != 6 || strings.Count(err.Error(), "orphan source ") != 1 {
					t.Fatalf("current selectors must produce six new sites and one stale site: %v; candidates=%+v", err, actual)
				}
				current.Operations[0].Sources = actual
				if err := Validate(current, actual, false); err != nil {
					t.Fatalf("reconciled replacements: %v", err)
				}
			}
		})
	}
}

func TestDiscoverIgnoresNonDefinitions(t *testing.T) {
	root := fstest.MapFS{
		"internal/web/server.go":                    {Data: []byte(`package web; func warn() { logger.Warn("x", slog.Any("key",value)) }`)},
		"internal/web/server_test.go":               {Data: []byte(`this is deliberately not Go`)},
		"internal/web/templates/generated_templ.go": {Data: []byte(`not Go`)},
		"web/conversation/src/api.test.ts":          {Data: []byte(`fetch('/test')`)},
		"web/conversation/src/node_modules/api.ts":  {Data: []byte(`fetch('/dependency')`)},
		"web/conversation/src/api.ts": {Data: []byte(`// fetch('/comment')
function send<A>(schema: A, path: string) { return schema; }
const action = "local"; if (action === "local") {};
const el = <a href="/existing">Read</a>;`)},
	}
	sites, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 || sites[0].Definition != `href="/existing"` {
		t.Fatalf("unexpected candidates: %+v", sites)
	}
}
