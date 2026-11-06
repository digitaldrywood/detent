package hubserver

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProjectCapacityClaims(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared host %t", shared), func(t *testing.T) {
			now := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
			cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}
			first := newNativeFixture(t, openTestService(t, cfg), "", "first-project")
			other := newNativeFixture(t, first.service, first.project.OrganizationID, "other-project")
			r := prepareRunner(t, first, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.redemption.Capacity = 8
			r.enroll(t)
			var second runnerFixture
			if shared {
				second = sharedRunner(t, r)
				requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, first.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", second.redemption.Credential, map[string]any{"capacity": 8, "version": "test", "backend_isolation": r.redemption.BackendIsolation}), http.StatusOK)
			} else {
				second = prepareRunner(t, first, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				second.redemption.Capacity = 8
				second.enroll(t)
			}
			for _, f := range []nativeFixture{first, other} {
				approveHubTestPolicy(t, first.service, f.base+"/policy", hubTestPolicy())
			}
			read := func(runner runnerFixture) runnerauth.Runner {
				t.Helper()
				response := performHubAPIRequest(t, first.service, http.MethodGet, runner.identityPath()+"/routing", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var stored runnerauth.Runner
				decodeHubResponse(t, response, &stored)
				return stored
			}
			change := func(runner runnerFixture, total int, limits map[tracker.ProjectID]int) {
				t.Helper()
				stored := read(runner)
				request := map[string]any{"expected_revision": stored.Revision, "display_name": stored.DisplayName, "state": stored.State, "capacity_limit": total, "project_ids": []tracker.ProjectID{first.project.ID, other.project.ID}, "idempotency_key": newNativeID("routing")}
				if limits != nil {
					request["project_capacity_limits"] = limits
				}
				requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPut, runner.identityPath()+"/routing", testHubAdminToken, request), http.StatusOK)
			}
			change(r, 8, map[tracker.ProjectID]int{first.project.ID: 4})
			change(second, 8, map[tracker.ProjectID]int{first.project.ID: 1, other.project.ID: 2})
			type claimed struct {
				claim tracker.NativeClaim
				lease tracker.NativeLease
				base  string
			}
			claim := func(runner runnerFixture, f nativeFixture, name string, want int) claimed {
				t.Helper()
				item := f.create(t, name)
				request := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: runner.binding.MachineID, SessionID: name, TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				response := performHubAPIRequest(t, first.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, request)
				requireNativeStatus(t, response, want)
				result := claimed{claim: request, base: f.base}
				if want == http.StatusOK {
					decodeHubResponse(t, response, &result.lease)
				}
				return result
			}
			release := func(runner runnerFixture, work claimed) {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, work.base+"/leases/"+string(work.lease.ID)+"/release", runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: work.lease.FencingToken, Reason: "released"}), http.StatusNoContent)
			}
			sibling := claim(second, first, "second-runner-first", http.StatusOK)
			claim(second, first, "second-runner-limit", http.StatusConflict)
			release(second, sibling)
			race := func(f nativeFixture, prefix string) []claimed {
				t.Helper()
				requests := make([]tracker.NativeClaim, 8)
				for i := range requests {
					item := f.create(t, fmt.Sprintf("%s-%d", prefix, i))
					requests[i] = tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: r.binding.MachineID, SessionID: fmt.Sprintf("%s-%d", prefix, i), TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				}
				start := make(chan struct{})
				results := make(chan claimed, len(requests))
				var workers sync.WaitGroup
				for _, request := range requests {
					workers.Go(func() {
						<-start
						response := performHubAPIRequest(t, first.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, request)
						if response.Code == http.StatusConflict {
							return
						}
						requireNativeStatus(t, response, http.StatusOK)
						var lease tracker.NativeLease
						decodeHubResponse(t, response, &lease)
						results <- claimed{claim: request, lease: lease, base: f.base}
					})
				}
				close(start)
				workers.Wait()
				close(results)
				var winners []claimed
				for result := range results {
					winners = append(winners, result)
				}
				if len(winners) != 4 {
					t.Fatalf("%s winners=%d, want 4", prefix, len(winners))
				}
				return winners
			}
			winners := race(first, "first-race")
			stored := read(r)
			if stored.Used != 4 || stored.ProjectUsed[first.project.ID] != 4 {
				t.Fatalf("first slots=%+v", stored)
			}
			runtime := performHubAPIRequest(t, first.service, http.MethodGet, first.base+"/work-items/"+string(winners[0].claim.WorkItemID)+"/runtime", first.token, nil)
			requireNativeStatus(t, runtime, http.StatusOK)
			var evidence tracker.NativeRuntimeEvidence
			decodeHubResponse(t, runtime, &evidence)
			available := -1
			for _, capacity := range evidence.Capacity {
				if capacity.RunnerID == r.binding.RunnerID {
					available = capacity.Available
				}
			}
			if available != 0 {
				t.Fatalf("runtime ignored remaining project headroom: available=%d", available)
			}
			change(r, 3, map[tracker.ProjectID]int{first.project.ID: 3})
			claim(r, first, "lowered-project", http.StatusConflict)
			claim(r, other, "lowered-total", http.StatusConflict)
			for _, work := range winners {
				requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, work.base+"/claims", r.redemption.Credential, work.claim), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, work.base+"/leases/"+string(work.lease.ID)+"/renew", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: work.lease.FencingToken, TTLSeconds: 300}), http.StatusOK)
			}
			change(r, 8, map[tracker.ProjectID]int{first.project.ID: 4})
			others := race(other, "other-race")
			claim(r, first, "ninth-first", http.StatusConflict)
			claim(r, other, "ninth-other", http.StatusConflict)
			if stored = read(r); stored.Used != 8 || stored.ProjectUsed[first.project.ID] != 4 || stored.ProjectUsed[other.project.ID] != 4 {
				t.Fatalf("combined slots=%+v", stored)
			}
			if shared {
				claim(second, other, "shared-host-ninth", http.StatusConflict)
			} else {
				independent := claim(second, first, "independent-host", http.StatusOK)
				release(second, independent)
			}
			change(r, 0, map[tracker.ProjectID]int{first.project.ID: 0})
			requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, winners[0].base+"/leases/"+string(winners[0].lease.ID)+"/renew", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: winners[0].lease.FencingToken, TTLSeconds: 300}), http.StatusOK)
			change(r, 8, nil)
			release(r, others[0])
			claim(r, first, "zero-project", http.StatusConflict)
			replacement := claim(r, other, "zero-project-other-starts", http.StatusOK)
			release(r, replacement)
			if err := first.service.Close(); err != nil {
				t.Fatal(err)
			}
			first.service = openTestService(t, cfg)
			other.service = first.service
			if stored = read(r); stored.ProjectCapacityLimits[first.project.ID] != 0 || stored.ProjectUsed[first.project.ID] != 4 {
				t.Fatalf("restart lost limits or usage=%+v", stored)
			}
			siblingStored := read(second)
			if siblingStored.ProjectCapacityLimits[first.project.ID] != 1 || siblingStored.ProjectCapacityLimits[other.project.ID] != 2 {
				t.Fatalf("runner limits crossed identities=%+v", siblingStored)
			}
			expired := winners[0]
			if _, err := first.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(now), expired.lease.ID); err != nil {
				t.Fatal(err)
			}
			renewal := tracker.NativeLeaseMutation{FencingToken: expired.lease.FencingToken, TTLSeconds: 300}
			renewPath := expired.base + "/leases/" + string(expired.lease.ID) + "/renew"
			requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, renewPath, r.redemption.Credential, renewal), http.StatusConflict)
			change(r, 8, map[tracker.ProjectID]int{})
			if stored = read(r); len(stored.ProjectCapacityLimits) != 0 || len(stored.ProjectIDs) != 2 {
				t.Fatalf("unsetting changed access=%+v", stored)
			}
			requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPost, renewPath, r.redemption.Credential, renewal), http.StatusOK)
			claim(r, first, "inherited-total", http.StatusOK)
			claim(r, other, "inherited-ninth", http.StatusConflict)
			expired = winners[1]
			if _, err := first.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(now), expired.lease.ID); err != nil {
				t.Fatal(err)
			}
			item := first.create(t, "renewal-race-contender")
			contender := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: r.binding.MachineID, SessionID: "renewal-race-contender", TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			start := make(chan struct{})
			statuses := make(chan int, 2)
			var workers sync.WaitGroup
			workers.Go(func() {
				<-start
				statuses <- performHubAPIRequest(t, first.service, http.MethodPost, expired.base+"/leases/"+string(expired.lease.ID)+"/renew", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: expired.lease.FencingToken, TTLSeconds: 300}).Code
			})
			workers.Go(func() {
				<-start
				statuses <- performHubAPIRequest(t, first.service, http.MethodPost, first.base+"/claims", r.redemption.Credential, contender).Code
			})
			close(start)
			workers.Wait()
			close(statuses)
			renewed := 0
			for status := range statuses {
				if status == http.StatusOK {
					renewed++
				} else if status != http.StatusConflict {
					t.Fatalf("renewal race status=%d", status)
				}
			}
			if renewed != 1 || read(r).Used != 8 {
				t.Fatalf("expired renewal raced beyond final slot: winners=%d", renewed)
			}

		})
	}
}

func TestRunnerProjectCapacityAdministration(t *testing.T) {
	f := newNativeFixture(t, nil, "", "project-capacity-admin")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-capacity-admin")
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations (id, name, created_at) VALUES ('org_foreign_capacity', 'Foreign organization', ?)", testTimestamp); err != nil {
		t.Fatal(err)
	}
	foreign := newNativeFixture(t, f.service, "org_foreign_capacity", "foreign-capacity-admin")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	base := map[string]any{"expected_revision": 1, "display_name": "Runner", "state": "active", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}, "idempotency_key": "limits"}
	for _, test := range []struct {
		name             string
		limit            any
		project          tracker.ProjectID
		key, token, path string
		want             int
	}{
		{name: "negative", limit: -1, want: 422},
		{name: "fractional", limit: 1.5, want: 422},
		{name: "out of range", limit: 10001, want: 422},
		{name: "string", limit: "4", want: 422},
		{name: "unauthorized project", project: other.project.ID, limit: 4, want: 422},
		{name: "cross organization project", project: foreign.project.ID, limit: 4, want: 422},
		{name: "missing idempotency", key: " ", limit: 4, want: 422},
		{name: "worker", token: r.redemption.Credential, limit: 4, want: 403},
		{name: "cross organization runner", path: "/api/v2/organizations/" + string(foreign.project.OrganizationID) + "/runners/" + r.binding.RunnerID + "/routing", limit: 4, want: 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := maps.Clone(base)
			project := test.project
			if project == "" {
				project = f.project.ID
			}
			request["project_capacity_limits"] = map[tracker.ProjectID]any{project: test.limit}
			if test.key != "" {
				request["idempotency_key"] = test.key
			}
			token := test.token
			if token == "" {
				token = testHubAdminToken
			}
			path := test.path
			if path == "" {
				path = r.identityPath() + "/routing"
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, path, token, request), test.want)
		})
	}
	base["project_ids"] = []tracker.ProjectID{f.project.ID, other.project.ID}
	base["project_capacity_limits"] = map[tracker.ProjectID]int{f.project.ID: 0, other.project.ID: 10000}
	response := performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, base)
	requireNativeStatus(t, response, http.StatusOK)
	var stored runnerauth.Runner
	decodeHubResponse(t, response, &stored)
	if stored.Revision != 2 || stored.ProjectCapacityLimits[f.project.ID] != 0 || stored.ProjectCapacityLimits[other.project.ID] != 10000 || stored.State != "active" || stored.CapacityLimit != 2 {
		t.Fatalf("limits readback=%+v", stored)
	}
	for _, test := range []struct {
		name    string
		limits  map[tracker.ProjectID]int
		include bool
	}{
		{name: "omitted limits"},
		{name: "null limits", include: true},
		{name: "retained limits", limits: stored.ProjectCapacityLimits, include: true},
	} {
		t.Run("remove access with "+test.name, func(t *testing.T) {
			removal := maps.Clone(base)
			removal["expected_revision"] = stored.Revision
			removal["idempotency_key"] = test.name
			removal["project_ids"] = []tracker.ProjectID{f.project.ID}
			delete(removal, "project_capacity_limits")
			if test.include {
				removal["project_capacity_limits"] = test.limits
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, removal), http.StatusUnprocessableEntity)
			response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var retained runnerauth.Runner
			decodeHubResponse(t, response, &retained)
			if retained.Revision != stored.Revision || !maps.Equal(retained.ProjectCapacityLimits, stored.ProjectCapacityLimits) || len(retained.ProjectIDs) != 2 {
				t.Fatalf("refused access removal mutated routing=%+v", retained)
			}
		})
	}
	retry := performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, base)
	requireNativeStatus(t, retry, http.StatusOK)
	if !bytes.Equal(bytes.TrimSpace(response.Body.Bytes()), bytes.TrimSpace(retry.Body.Bytes())) {
		t.Fatal("routing retry changed its receipt")
	}
	changed := maps.Clone(base)
	changed["project_capacity_limits"] = map[tracker.ProjectID]int{}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, changed), http.StatusConflict)
	changed["idempotency_key"] = "stale-revision"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, changed), http.StatusConflict)
	capacityResponse := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/capacity", testHubAdminToken, nil)
	requireNativeStatus(t, capacityResponse, http.StatusOK)
	var capacity runnerauth.CapacityView
	decodeHubResponse(t, capacityResponse, &capacity)
	zero, unknown := capacity.Projects[f.project.ID], capacity.Projects[other.project.ID]
	if capacity.Status != "unknown" || capacity.Effective != nil || zero.Effective == nil || *zero.Effective != 0 || zero.Applied == nil || *zero.Applied != 0 || unknown.Effective != nil {
		t.Fatalf("project override forged effective total evidence=%+v", capacity)
	}
	heartbeat := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"capacity": 2, "version": "test"})
	requireNativeStatus(t, heartbeat, http.StatusOK)
	var snapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, heartbeat, &snapshot)
	if !maps.Equal(snapshot.Routing.ProjectCapacityLimits, stored.ProjectCapacityLimits) {
		t.Fatal("heartbeat lost project overrides")
	}
	changed["expected_revision"] = 2
	delete(changed, "project_capacity_limits")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, changed), http.StatusOK)
	read := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
	requireNativeStatus(t, read, http.StatusOK)
	var after runnerauth.Runner
	decodeHubResponse(t, read, &after)
	if !maps.Equal(after.ProjectCapacityLimits, stored.ProjectCapacityLimits) {
		t.Fatal("omitted overrides were cleared")
	}
	changed["expected_revision"] = after.Revision
	changed["idempotency_key"] = "clear-project-limits"
	changed["project_ids"] = []tracker.ProjectID{f.project.ID}
	changed["project_capacity_limits"] = map[tracker.ProjectID]int{}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, changed), http.StatusOK)
	cleared := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
	requireNativeStatus(t, cleared, http.StatusOK)
	var removed runnerauth.Runner
	decodeHubResponse(t, cleared, &removed)
	if len(removed.ProjectCapacityLimits) != 0 || len(removed.ProjectIDs) != 1 || removed.ProjectIDs[0] != f.project.ID {
		t.Fatalf("explicit override removal did not remove access atomically=%+v", removed)
	}
	delete(changed, "project_capacity_limits")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, changed), http.StatusConflict)
	changed["expected_revision"] = removed.Revision
	changed["idempotency_key"] = "restore-project-access"
	changed["project_ids"] = []tracker.ProjectID{f.project.ID, other.project.ID}
	restored := performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, changed)
	requireNativeStatus(t, restored, http.StatusOK)
	var granted runnerauth.Runner
	decodeHubResponse(t, restored, &granted)
	if len(granted.ProjectCapacityLimits) != 0 || len(granted.ProjectIDs) != 2 {
		t.Fatalf("re-added access revived stale ceilings=%+v", granted)
	}
}
