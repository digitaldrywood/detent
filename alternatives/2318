package runnerfleet_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRetainedLeaseClaimEligibility(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                       string
		drain, expire, revoke, reclaim, wrongFence bool
		want                                       int
	}{
		{name: "live foreign lease", want: http.StatusConflict},
		{name: "draining foreign lease", drain: true, want: http.StatusConflict},
		{name: "foreign lease expires", expire: true, want: http.StatusOK},
		{name: "revocation waits for the lease to expire", revoke: true, want: http.StatusOK},
		{name: "owner reclaims with original fence", reclaim: true, want: http.StatusOK},
		{name: "draining owner reclaims", drain: true, reclaim: true, want: http.StatusOK},
		{name: "wrong fence cannot reclaim", reclaim: true, wrongFence: true, want: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 8, 15, 7, 57, 0, time.UTC)
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{Clock: func() time.Time { return now }})
			owner := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			owner.Enroll(t)
			owner.ReportCapacity(t)
			other := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			other.Enroll(t)
			other.ReportCapacity(t)
			descriptor := hubfixture.Policy()
			hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", descriptor)
			item := f.Create(t, "Restart ownership")
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: item.WorkItemID, MachineID: owner.Binding.MachineID, SessionID: "original", TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", owner.Redemption.Credential, claim)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			hubfixture.Decode(t, response, &lease)
			if test.drain {
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, owner.IdentityPath()+"/routing", hubfixture.AdminToken, runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "draining", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.Project.ID}}}), http.StatusOK)
			}
			if test.revoke {
				refused := f.Hub.Request(t, http.MethodDelete, owner.IdentityPath(), hubfixture.AdminToken, nil)
				hubfixture.RequireStatus(t, refused, http.StatusUnprocessableEntity)
				var failure struct{ Code, Message string }
				hubfixture.Decode(t, refused, &failure)
				if failure.Code != "invalid_request" || failure.Message != "This runner has active work. Drain it and wait for its leases to finish or expire before removing it." {
					t.Fatalf("revocation with a live lease = %+v", failure)
				}
			}
			if test.expire || test.revoke {
				now = now.Add(600 * time.Second)
				other.ReportCapacity(t)
			}
			if test.revoke {
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodDelete, owner.IdentityPath(), hubfixture.AdminToken, nil), http.StatusNoContent)
			}
			if test.reclaim {
				token := lease.FencingToken
				if test.wrongFence {
					token++
				}
				response = f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(lease.ID)+"/renew", owner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: token, TTLSeconds: 600})
			} else {
				claim.MachineID, claim.SessionID = other.Binding.MachineID, "replacement"
				response = f.Hub.Request(t, http.MethodPost, f.Base+"/claims", other.Redemption.Credential, claim)
			}
			hubfixture.RequireStatus(t, response, test.want)
			if test.want == http.StatusOK {
				var result tracker.NativeLease
				hubfixture.Decode(t, response, &result)
				if test.reclaim {
					if result.ID != lease.ID || result.FencingToken != lease.FencingToken || result.SessionID != lease.SessionID {
						t.Fatalf("reclaim replaced authority: %+v", result)
					}
				} else {
					if result.FencingToken <= lease.FencingToken {
						t.Fatalf("replacement reused fence: %+v", result)
					}
					staleStatus := http.StatusConflict
					if test.revoke {
						staleStatus = http.StatusUnauthorized
					}
					hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(lease.ID)+"/renew", owner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 600}), staleStatus)
				}
			}
		})
	}
}
