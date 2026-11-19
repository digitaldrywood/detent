package runnerfleet_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerIsolationLeaseLifecycle(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	for _, test := range []struct {
		name, initial, changed string
		legacy                 bool
	}{
		{"sandbox remains sandbox", isolation.Sandbox, isolation.NativeTrusted, false},
		{"trusted remains trusted", isolation.NativeTrusted, isolation.Sandbox, false},
		{"legacy follows trusted", isolation.Sandbox, isolation.NativeTrusted, true},
		{"legacy follows sandbox", isolation.NativeTrusted, isolation.Sandbox, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			organization := h.CreateOrganization(t, test.name)
			f := hubfixture.NewNative(t, h, organization, "Example project")
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.Redemption.IsolationTier = test.initial
			r.Enroll(t)
			r.ReportCapacity(t)
			issue := f.Create(t, "Queued work")
			claimPath := f.Base + "/claims"
			token := r.Redemption.Credential
			claim := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: issue.WorkItemID, MachineID: r.Binding.MachineID, SessionID: "pinned", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			readLease := func(path string, payload any, tier string) tracker.NativeLease {
				t.Helper()
				response := h.Request(t, http.MethodPost, path, token, payload)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				hubfixture.Decode(t, response, &lease)
				if lease.IsolationPolicy == nil || lease.IsolationPolicy.Tier != tier {
					t.Fatalf("lease isolation = %+v, want %s", lease.IsolationPolicy, tier)
				}
				return lease
			}
			lease := readLease(claimPath, claim, test.initial)
			storedTier, renewalTier := &test.initial, test.initial
			if test.legacy {
				if _, err := h.Admin.ExecContext(t.Context(), `UPDATE leases SET isolation_tier=NULL WHERE organization_id=$1 AND lease_id=$2`, organization, lease.ID); err != nil {
					t.Fatal(err)
				}
				storedTier, renewalTier = nil, test.changed
			}
			checkStoredLease := func(id string, tier *string, requireReleased bool) {
				t.Helper()
				if err := h.Repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
					stored, err := reader.Lease(t.Context(), id)
					if err != nil {
						return err
					}
					if requireReleased && stored.ReleasedAt == nil || !reflect.DeepEqual(stored.IsolationTier, tier) {
						t.Errorf("stored lease %s has unexpected release or isolation state: %+v", id, stored)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.Project.ID}, IsolationTier: test.changed}}
			hubfixture.RequireStatus(t, h.Request(t, http.MethodPut, r.IdentityPath()+"/routing", h.AdminToken(), change), http.StatusOK)
			leasePath := f.Base + "/leases/" + string(lease.ID)
			mutation := tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}
			for _, result := range []tracker.NativeLease{readLease(claimPath, claim, renewalTier), readLease(leasePath+"/renew", mutation, renewalTier)} {
				if result.ID != lease.ID {
					t.Fatalf("existing lease was replaced: got %s, want %s", result.ID, lease.ID)
				}
			}
			start := hubfixture.StartedEvent(lease)
			finish := start
			finish.Type, finish.IdempotencyKey = "run.finished", "finish"
			finish.Data.Sequence, finish.Data.Outcome = 2, "succeeded"
			finish.Data.CompletionBody = "Completed attempt details"
			itemPath := f.Base + "/work-items/" + string(issue.WorkItemID)
			for _, event := range []tracker.NativeRunEvent{start, finish} {
				hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, itemPath+"/events", token, event), http.StatusOK)
				checkStoredLease(string(lease.ID), storedTier, false)
			}
			hubfixture.RequireStatus(t, h.Request(t, http.MethodGet, itemPath+"/attempts", token, nil), http.StatusOK)
			hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, leasePath+"/release", token, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			claim.SessionID = "after-change"
			next := readLease(claimPath, claim, test.changed)
			if next.ID == lease.ID {
				t.Fatal("new claim reused the released lease")
			}
			checkStoredLease(string(lease.ID), storedTier, true)
			checkStoredLease(string(next.ID), &test.changed, false)
		})
	}
}

type runnerIsolationClaimKey struct{}

type runnerIsolationClaimBarrier struct {
	*hubtest.Details
	ready   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *runnerIsolationClaimBarrier) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	if ctx.Value(runnerIsolationClaimKey{}) == b {
		var event tracker.CollaborationEvent
		if json.Unmarshal(data, &event) == nil && event.Type == "scheduler.decision" && event.Data.Decision != nil && event.Data.Decision.Outcome == "claimed" {
			b.once.Do(func() {
				close(b.ready)
				select {
				case <-b.release:
				case <-ctx.Done():
				}
			})
			if err := ctx.Err(); err != nil {
				return attachment.ArchiveReference{}, err
			}
		}
	}
	return b.Details.StoreDetail(ctx, organization, data)
}

func TestRunnerIsolationRefusalAfterHeartbeatDuringClaim(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Now().UTC().Truncate(time.Microsecond))
	barrier := &runnerIsolationClaimBarrier{Details: hubtest.NewDetails(), ready: make(chan struct{}), release: make(chan struct{})}
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{Clock: clock.Now, RunDetails: barrier})
	h := f.Hub
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.Enroll(t)
	r.ReportCapacity(t)
	issue := f.Create(t, "Queued work")
	claim := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: issue.WorkItemID, MachineID: r.Binding.MachineID, SessionID: "heartbeat-during-claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	preview := h.Request(t, http.MethodPost, f.Base+"/claims/preview", r.Redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: claim})
	hubfixture.RequireStatus(t, preview, http.StatusOK)
	var candidates tracker.NativeCapacityPage
	hubfixture.Decode(t, preview, &candidates)
	listed := false
	for _, candidate := range candidates.Items {
		listed = listed || candidate.WorkItemID == issue.WorkItemID
	}
	if !listed {
		t.Fatal("work item was missing from the claim preview before sandbox withdrawal")
	}
	request := hubfixture.NewRequest(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, claim)
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), runnerIsolationClaimKey{}, barrier))
	done := make(chan struct{})
	response := httptest.NewRecorder()
	go func() {
		defer close(done)
		h.Handler().ServeHTTP(response, request.WithContext(ctx))
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-barrier.ready:
	case <-done:
		t.Fatalf("claim returned before preparing its decision: %s", response.Body.String())
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("claim did not prepare its decision")
	}
	clock.Advance(time.Second)
	r.Redemption.BackendIsolation = isolation.Report{"codex": {isolation.NativeTrusted}}
	r.ReportCapacity(t)
	close(barrier.release)
	<-done
	hubfixture.RequireStatus(t, response, http.StatusConflict)
	var failure hubapi.NativeError
	hubfixture.Decode(t, response, &failure)
	if failure.Code != "no_claimable_work" {
		t.Fatalf("refusal after sandbox withdrawal = %q, want no_claimable_work", failure.Code)
	}
}
