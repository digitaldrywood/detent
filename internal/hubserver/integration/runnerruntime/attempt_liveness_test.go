package runnerruntime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/executionapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAttemptLivenessFollowsOwningRunnerHeartbeat(t *testing.T) {
	cfg := hubfixture.StartupConfig()
	var offset atomic.Int64
	cfg.Clock = func() time.Time { return time.Now().UTC().Add(time.Duration(offset.Load())) }
	f := hubfixture.SeedLeasedRunners(t, hubfixture.OpenStartup(t, cfg), hubfixture.StartupOrganization)
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
	lease := f.Leases[0]
	projectedExpiry := f.Hub.Now().Add(45 * time.Second).Truncate(time.Microsecond)
	if _, err := f.Admin.ExecContext(t.Context(), "UPDATE leases SET expires_at=$3 WHERE organization_id=$1 AND lease_id=$2", f.Organization, lease.ID, projectedExpiry); err != nil {
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
		change    string
	}{
		{"fresh", now, "running", "available", "Implementing", true, ""},
		{"stale", now.Add(-runnerauth.HeartbeatTimeout), "interrupted", "expired", "Interrupted", false, ""},
		{"fresh_again", now, "running", "available", "Implementing", true, ""},
		{"renewed_past_cached_expiry", now, "running", "available", "Implementing", true, "renew"},
		{"interrupted_while_lease_is_retained", now, "interrupted", "available", "Interrupted", false, "interrupt"},
		{"released_while_owner_is_fresh", now, "interrupted", "expired", "Interrupted", false, "release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			heartbeat := tc.heartbeat
			switch tc.change {
			case "renew":
				renewedAt := projectedExpiry.Add(-time.Second)
				if _, err := f.Admin.ExecContext(t.Context(), "UPDATE leases SET renewed_at=$3,expires_at=$4 WHERE organization_id=$1 AND lease_id=$2", f.Organization, lease.ID, renewedAt, projectedExpiry.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				offset.Store(int64(time.Until(projectedExpiry.Add(time.Second))))
				heartbeat = f.Hub.Now()
			case "interrupt":
				heartbeat = f.Hub.Now()
				if _, err := f.Admin.ExecContext(t.Context(), "UPDATE native_attempts SET status='interrupted' WHERE organization_id=$1 AND id=$2", f.Organization, data.AttemptID); err != nil {
					t.Fatal(err)
				}
			case "release":
				heartbeat = f.Hub.Now()
				if _, err := f.Admin.ExecContext(t.Context(), "UPDATE leases SET released_at=$3 WHERE organization_id=$1 AND lease_id=$2", f.Organization, lease.ID, heartbeat); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.Admin.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=$3 WHERE organization_id=$1 AND runner_id=$2", f.Organization, f.Bindings[0].RunnerID, heartbeat); err != nil {
				t.Fatal(err)
			}
			wantRunning := 1
			if tc.running {
				wantRunning++
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
					Cards    []tracker.NativeBoardCard
					Work     *tracker.NativeWorkSummary
					Projects []struct{ Work *tracker.NativeWorkSummary }
				}
				hubfixture.Decode(t, response, &page)
				work := page.Work
				if len(page.Projects) == 1 {
					work = page.Projects[0].Work
				}
				requireRunningCount(t, read.name, work, wantRunning)
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
						if running != (card.LivenessExpiresAt != nil) {
							t.Errorf("%s item %s deadline = %v, want live=%t", read.name, lease.WorkItemID, card.LivenessExpiresAt, running)
						}
						if tc.change == "renew" && i == 0 && (card.Execution == nil || !card.Execution.ExpiresAt.Equal(projectedExpiry.Add(time.Hour))) {
							t.Errorf("%s did not return renewed lease expiry: %+v", read.name, card.Execution)
						}
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
			limited := f.Hub.Request(t, http.MethodGet, base+"/projects/prj_runner/work-items?include=board&state=Todo&limit=1", f.Tokens[0], nil)
			hubfixture.RequireStatus(t, limited, http.StatusOK)
			var page tracker.NativeIssuePage
			hubfixture.Decode(t, limited, &page)
			requireRunningCount(t, "limited project board", page.Work, wantRunning)
			targeted := hosted.Request(t, viewer, http.MethodGet, base+"/work-board?project=prj_runner&item="+string(lease.WorkItemID), nil)
			hubfixture.RequireStatus(t, targeted, http.StatusOK)
			var slice struct {
				Cards    []tracker.NativeBoardCard
				Projects []struct{ Work *tracker.NativeWorkSummary }
			}
			hubfixture.Decode(t, targeted, &slice)
			if len(slice.Cards) != 1 || len(slice.Projects) != 1 {
				t.Fatalf("targeted board returned %d cards in %d projects", len(slice.Cards), len(slice.Projects))
			}
			requireRunningCount(t, "targeted work board", slice.Projects[0].Work, wantRunning)
			runtime := explanation.NativeRuntime
			if tc.change == "release" {
				if runtime == nil || runtime.CurrentLease != nil || runtime.Attempt == nil || runtime.Attempt.Current {
					t.Fatalf("explain still shows released authority: %+v", runtime)
				}
				return
			}
			if runtime == nil || runtime.CurrentLease == nil || runtime.CurrentLease.ID != f.Leases[0].ID || runtime.CurrentLease.FencingToken != f.Leases[0].FencingToken {
				t.Fatal("explain lost current lease authority")
			}
			if runtime.Attempt == nil || !runtime.Attempt.Current || runtime.Attempt.RuntimeFreshness != tc.freshness {
				t.Errorf("explain runtime attempt = %+v, want current lease and %s runtime evidence", runtime.Attempt, tc.freshness)
			}
		})
	}
}

func requireRunningCount(t *testing.T, route string, work *tracker.NativeWorkSummary, want int) {
	t.Helper()
	if work != nil {
		for _, lane := range work.Lanes {
			if lane.State == "Todo" && lane.Running == want {
				return
			}
		}
	}
	t.Errorf("%s work = %+v, want %d running in Todo", route, work, want)
}

type waitingRuntimeSnapshot struct {
	tenantstore.Repository
	snapshot tenantstore.SnapshotRepository
	entered  chan struct{}
	resume   chan struct{}
}

func (w waitingRuntimeSnapshot) ReadTenantSnapshot(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	close(w.entered)
	select {
	case <-w.resume:
		return w.snapshot.ReadTenantSnapshot(ctx, org, workload, read)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestPostgresRuntimeObservesHeartbeatAndRenewalCommittedBeforeSnapshot(t *testing.T) {
	f := hubfixture.NewLeasedRunners(t)
	scope := hubscope.NativeScope{Organization: f.Organization, Project: "prj_runner"}
	lease := f.Leases[0]
	details := func(context.Context) attachment.DetailStore { return f.Hub.Config.RunDetails }
	attempts := executionapi.AttemptDeps{DetailsFor: details, Details: func() attachment.DetailStore { return details(t.Context()) }}
	base := f.Hub.Now().UTC().Truncate(time.Microsecond)
	for _, tc := range []struct {
		name  string
		renew bool
	}{
		{"heartbeat", false},
		{"heartbeat_and_renewal", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waiting := waitingRuntimeSnapshot{Repository: f.Hub.Repository, snapshot: f.Hub.Repository, entered: make(chan struct{}), resume: make(chan struct{})}
			var now atomic.Pointer[time.Time]
			now.Store(&base)
			deps := executionapi.RuntimeDeps{
				Repository: waiting, Now: func() time.Time { return *now.Load() },
				AuthorizeAccess: func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error { return nil },
				DetailsFor:      details, HydrateAttempt: attempts.HydrateNativeAttemptTenant,
				HydrateWorkItem: func(_ context.Context, item corestore.WorkItem, _ eventstore.CollaborationVersion) (tracker.NativeIssue, error) {
					return tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: item.NativeID}, State: "Todo"}, nil
				},
				HydrateRelationships: func(context.Context, hubscope.NativeScope, *tracker.NativeIssue) error { return nil },
				ReadSourceRecovery:   func(context.Context, hubscope.NativeScope, string, *tracker.NativeRuntimeEvidence) error { return nil },
			}
			type result struct {
				evidence tracker.NativeRuntimeEvidence
				err      error
			}
			done := make(chan result, 1)
			go func() {
				evidence, _, _, err := deps.ReadSelectedNativeRuntimeTenant(ctx, scope, string(lease.WorkItemID), executionapi.RuntimeAttemptSelector{Latest: true})
				done <- result{evidence, err}
			}()
			t.Cleanup(func() {
				cancel()
				if done != nil {
					<-done
				}
			})
			select {
			case <-waiting.entered:
			case premature := <-done:
				done = nil
				t.Fatalf("runtime ended before snapshot: %v", premature.err)
			}
			heartbeat := base.Add(time.Second)
			if _, err := f.Admin.ExecContext(ctx, "UPDATE runner_identities SET last_heartbeat_at=$3 WHERE organization_id=$1 AND runner_id=$2", f.Organization, f.Bindings[0].RunnerID, heartbeat); err != nil {
				t.Fatal(err)
			}
			if tc.renew {
				if _, err := f.Admin.ExecContext(ctx, "UPDATE leases SET renewed_at=$3,expires_at=$4 WHERE organization_id=$1 AND lease_id=$2", f.Organization, lease.ID, heartbeat, heartbeat.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			now.Store(&heartbeat)
			close(waiting.resume)
			got := <-done
			done = nil
			if got.err != nil {
				t.Fatal(got.err)
			}
			if evidence := got.evidence; !evidence.ObservedAt.Equal(heartbeat) || evidence.Attempt == nil || evidence.Attempt.Status != "running" || !evidence.Attempt.Current || evidence.CurrentLease == nil || evidence.CurrentLease.FencingToken != lease.FencingToken || !evidence.CurrentLease.ServerTime.Equal(heartbeat) {
				t.Fatalf("did not observe committed heartbeat and current authority: %+v", evidence)
			}
		})
	}
}
