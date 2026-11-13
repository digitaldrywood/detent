package runnerfleet_test

import (
	"net/http"
	"testing"
	"time"

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
		{name: "revocation immediately releases", revoke: true, want: http.StatusOK},
		{name: "owner reclaims with original fence", reclaim: true, want: http.StatusOK},
		{name: "draining owner reclaims", drain: true, reclaim: true, want: http.StatusOK},
		{name: "wrong fence cannot reclaim", reclaim: true, wrongFence: true, want: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 8, 15, 7, 57, 0, time.UTC)
			f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }})
			owner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			owner.enroll(t)
			owner.reportCapacity(t)
			other := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			other.enroll(t)
			other.reportCapacity(t)
			descriptor := hubTestPolicy()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			item := f.create(t, "Restart ownership")
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: item.WorkItemID, MachineID: owner.binding.MachineID, SessionID: "original", TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", owner.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			if test.drain {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, owner.identityPath()+"/routing", testHubAdminToken, runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "draining", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}), http.StatusOK)
			}
			if test.revoke {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, owner.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
			}
			if test.expire {
				now = now.Add(600 * time.Second)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(other.binding.MachineID)+"/heartbeat", other.redemption.Credential, map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": other.redemption.BackendIsolation}), http.StatusOK)
			}
			if test.reclaim {
				token := lease.FencingToken
				if test.wrongFence {
					token++
				}
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: token, TTLSeconds: 600})
			} else {
				claim.MachineID, claim.SessionID = other.binding.MachineID, "replacement"
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", other.redemption.Credential, claim)
			}
			requireNativeStatus(t, response, test.want)
			if test.want == http.StatusOK {
				var result tracker.NativeLease
				decodeHubResponse(t, response, &result)
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
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 600}), staleStatus)
				}
			}
		})
	}
}
