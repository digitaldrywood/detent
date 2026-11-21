package runnerruntime_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAttemptLivenessFollowsOwningRunnerHeartbeat(t *testing.T) {
	f := hubfixture.NewLeasedRunners(t)
	hosted := &hubfixture.Hosted{Hub: f.Hub, Provider: f.Hub.Config.Hosted.Provider.(*hubfixture.HostedProvider)}
	viewer := hosted.User(t, "viewer", "viewer", "viewer@example.test", "", "")
	hosted.Grant(t, viewer, "prj_runner", false, false)
	native, ctx := f.Client(t, 0)
	data := f.Data[0]
	data.Sequence++
	data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: f.Hub.Now().UTC()}
	if err := native.AppendEvent(ctx, f.Leases[0].WorkItemID, tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: "observe-running-attempt"}, Type: "run.observed", SchemaVersion: 1, Data: data,
	}); err != nil {
		t.Fatal(err)
	}
	f.Hub.RefreshBoard(t, f.Organization)
	base := hubfixture.OrganizationBase(f.Organization)
	var member hubtest.SeededMember
	if err := f.Hub.Repository.ReadTenant(t.Context(), f.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member.Member, err = r.AccessMember(t.Context(), viewer.Identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	token := hubtest.Credential(t, f.Hub.Repository, f.Organization, hubtest.CredentialOptions{Member: &member, KeyScope: "read", Projects: []tracker.ProjectID{"prj_runner"}}).Token
	call := hubfixture.MCP(t, f.Hub.Handler(), hubfixture.MCPPath(f.Organization), token)
	now := f.Hub.Now().UTC()
	for _, tc := range []struct {
		name      string
		heartbeat time.Time
		status    string
		freshness string
		line      string
		running   bool
	}{
		{"fresh", now, "running", "available", "Implementing", true},
		{"stale", now.Add(-runnerauth.HeartbeatTimeout), "interrupted", "expired", "Interrupted", false},
		{"fresh_again", now, "running", "available", "Implementing", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.Admin.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=$3 WHERE organization_id=$1 AND runner_id=$2", f.Organization, f.Bindings[0].RunnerID, tc.heartbeat); err != nil {
				t.Fatal(err)
			}
			for _, read := range []struct {
				name     string
				response *httptest.ResponseRecorder
			}{
				{"project board", f.Hub.Request(t, http.MethodGet, base+"/projects/prj_runner/work-items?include=board&limit=50", f.Tokens[0], nil)},
				{"work board", hosted.Request(t, viewer, http.MethodGet, base+"/work-board?project=prj_runner", nil)},
			} {
				response := read.response
				if response.Code != http.StatusOK {
					t.Errorf("%s: HTTP %d", read.name, response.Code)
					continue
				}
				var page struct {
					Cards []tracker.NativeBoardCard
				}
				hubfixture.Decode(t, response, &page)
				for i, lease := range f.Leases {
					status, line, running := tc.status, tc.line, tc.running
					if i != 0 {
						status, line, running = "running", "Running", true
					}
					found := false
					for _, card := range page.Cards {
						if card.Issue.WorkItemID != lease.WorkItemID {
							continue
						}
						found = true
						if card.Attempt == nil || card.Attempt.Status != status || (card.Execution != nil && card.Execution.Running) != running || card.Issue.State != "Todo" || card.StatusLine != line {
							t.Errorf("%s item %s: attempt=%+v execution=%+v lane=%q line=%q, want status=%s running=%t lane=Todo line=%q", read.name, lease.WorkItemID, card.Attempt, card.Execution, card.Issue.State, card.StatusLine, status, running, line)
						}
					}
					if !found {
						t.Errorf("%s omitted item %s", read.name, lease.WorkItemID)
					}
				}
			}
			reply := call("tools/call", operatortool.ExplainItem, map[string]any{"project_id": "prj_runner", "reference": "1"})
			var explanation explain.IssueExplanation
			if err := json.Unmarshal(hubfixture.MCPData(t, reply, false), &explanation); err != nil {
				t.Fatal(err)
			}
			if explanation.Attempt == nil || explanation.Attempt.Status != tc.status {
				t.Errorf("explain attempt = %+v, want status %s", explanation.Attempt, tc.status)
			}
			runtime := explanation.NativeRuntime
			if runtime == nil || runtime.CurrentLease == nil || runtime.CurrentLease.ID != f.Leases[0].ID || runtime.CurrentLease.FencingToken != f.Leases[0].FencingToken {
				t.Fatal("explain lost current lease authority")
			}
			if runtime.Attempt == nil || !runtime.Attempt.Current || runtime.Attempt.RuntimeFreshness != tc.freshness {
				t.Errorf("explain runtime attempt = %+v, want current lease and %s runtime evidence", runtime.Attempt, tc.freshness)
			}
		})
	}
}
