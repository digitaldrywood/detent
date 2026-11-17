package workspacefixture

import (
	"encoding/json"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type Hub struct {
	Client           *http.Client
	Reports          chan *workspacesession.Capabilities
	Released         chan struct{}
	releaseBarrier   chan struct{}
	once             sync.Once
	Claims           int
	WorkspaceSession string
	IssueSessions    []string
}
type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (h *Hub) Unblock() { h.once.Do(func() { close(h.releaseBarrier) }) }

func New(t *testing.T, identity runnerauth.File) *Hub {
	t.Helper()
	h := &Hub{Reports: make(chan *workspacesession.Capabilities, 10), Released: make(chan struct{}), releaseBarrier: make(chan struct{})}
	h.Client = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		var response any
		switch {
		case r.URL.Path == "/api/v2/capabilities":
			response = map[string]any{"version": "dev", "protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeWorkspaceCapability, tracker.NativeRunnerUpdateCapability, tracker.NativeProviderCapacityCapability}}
		case strings.HasSuffix(r.URL.Path, "/runners/"+identity.Identity.RunnerID):
			response = identity.Identity
		case strings.HasSuffix(r.URL.Path, "/update/receipt"):
			response = (*runnerauth.UpdateReceipt)(nil)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/prj_test"):
			response = tracker.NativeProject{Profile: "native"}
		case strings.HasSuffix(r.URL.Path, "/policy/observed"):
			w.WriteHeader(http.StatusNoContent)
			return w.Result(), nil
		case strings.HasSuffix(r.URL.Path, "/policy"):
			response = map[string]any{"policy": nil}
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			var report struct {
				WorkspaceCapabilities *workspacesession.Capabilities `json:"workspace_capabilities"`
			}
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Error(err)
			}
			h.Reports <- report.WorkspaceCapabilities
			response = runnerauth.RoutingSnapshot{RunnerID: identity.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Example runner", State: "active", CapacityLimit: 1}.Normalized()}
		case strings.HasSuffix(r.URL.Path, "/claims"):
			var claim tracker.NativeClaim
			if err := json.NewDecoder(r.Body).Decode(&claim); err != nil {
				t.Error(err)
			}
			if !strings.Contains(claim.SessionID, "-workspace-") {
				h.IssueSessions = append(h.IssueSessions, claim.SessionID)
				w.WriteHeader(http.StatusConflict)
				_, _ = w.WriteString(`{"code":"no_claimable_work"}`)
				return w.Result(), nil
			}
			h.Claims++
			h.WorkspaceSession = claim.SessionID
			if claim.TTLSeconds != 90 {
				t.Errorf("workspace TTL = %d", claim.TTLSeconds)
			}
			response = tracker.NativeLease{ID: "lease_workspace", WorkItemID: "wi_workspace", FencingToken: 1, IsolationPolicy: &isolation.Policy{}}
		case strings.HasSuffix(r.URL.Path, "/work-items/wi_workspace/workspace"):
			response = workspacesession.Session{ID: "ws_000000000000000000000000000000ff", State: workspacesession.StateClosed}
		case strings.HasSuffix(r.URL.Path, "/leases/lease_workspace/release"):
			close(h.Released)
			<-h.releaseBarrier
			w.WriteHeader(http.StatusNoContent)
			return w.Result(), nil
		default:
			t.Logf("unhandled: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return w.Result(), nil
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
		return w.Result(), nil
	})}

	return h
}
