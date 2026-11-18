package runnerfleet_test

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProjectCapacityClaims(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared host %t", shared), func(t *testing.T) {
			now := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
			cfg := hubfixture.Config{Clock: func() time.Time { return now }}
			first := hubfixture.NewNative(t, hubfixture.Open(t, cfg), "", "first-project")
			other := hubfixture.NewNative(t, first.Hub, first.Project.OrganizationID, "other-project")
			r := hubfixture.PrepareRunner(t, first, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.Redemption.Capacity = 8
			r.Enroll(t)
			r.ReportCapacity(t)
			var second hubfixture.Runner
			if shared {
				second = hubfixture.SharedRunner(t, r)
				hubfixture.RequireStatus(t, first.Hub.Request(t, http.MethodPost, first.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", second.Redemption.Credential, map[string]any{"display_name": "Runner", "capacity": 8, "version": "test", "backend_isolation": r.Redemption.BackendIsolation,
					"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: now}}), http.StatusOK)
			} else {
				second = hubfixture.PrepareRunner(t, first, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				second.Redemption.Capacity = 8
				second.Enroll(t)
				second.ReportCapacity(t)
			}
			for _, f := range []hubfixture.Native{first, other} {
				hubfixture.ApprovePolicy(t, first.Hub, f.Base+"/policy", hubfixture.Policy())
			}
			read := func(runner hubfixture.Runner) runnerauth.Runner {
				t.Helper()
				response := first.Hub.Request(t, http.MethodGet, runner.IdentityPath()+"/routing", hubfixture.AdminToken, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var stored runnerauth.Runner
				hubfixture.Decode(t, response, &stored)
				return stored
			}
			change := func(runner hubfixture.Runner, total int, limits map[tracker.ProjectID]int) {
				t.Helper()
				stored := read(runner)
				request := map[string]any{"expected_revision": stored.Revision, "display_name": stored.DisplayName, "state": stored.State, "capacity_limit": total, "project_ids": []tracker.ProjectID{first.Project.ID, other.Project.ID}, "idempotency_key": hubapi.NewID("routing")}
				if limits != nil {
					request["project_capacity_limits"] = limits
				}
				hubfixture.RequireStatus(t, first.Hub.Request(t, http.MethodPut, runner.IdentityPath()+"/routing", hubfixture.AdminToken, request), http.StatusOK)
			}
			change(r, 8, map[tracker.ProjectID]int{first.Project.ID: 4})
			change(second, 8, map[tracker.ProjectID]int{first.Project.ID: 1, other.Project.ID: 2})
			type claimed struct {
				claim tracker.NativeClaim
				lease tracker.NativeLease
				base  string
			}
			claim := func(runner hubfixture.Runner, f hubfixture.Native, name string, want int) claimed {
				t.Helper()
				item := f.Create(t, name)
				request := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: item.WorkItemID, MachineID: runner.Binding.MachineID, SessionID: name, TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				response := first.Hub.Request(t, http.MethodPost, f.Base+"/claims", runner.Redemption.Credential, request)
				hubfixture.RequireStatus(t, response, want)
				result := claimed{claim: request, base: f.Base}
				if want == http.StatusOK {
					hubfixture.Decode(t, response, &result.lease)
				}
				return result
			}
			release := func(runner hubfixture.Runner, work claimed) {
				t.Helper()
				hubfixture.RequireStatus(t, first.Hub.Request(t, http.MethodPost, work.base+"/leases/"+string(work.lease.ID)+"/release", runner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: work.lease.FencingToken, Reason: "released"}), http.StatusNoContent)
			}
			sibling := claim(second, first, "second-runner-first", http.StatusOK)
			claim(second, first, "second-runner-limit", http.StatusConflict)
			release(second, sibling)
			race := func(f hubfixture.Native, prefix string) []claimed {
				t.Helper()
				requests := make([]tracker.NativeClaim, 8)
				for i := range requests {
					item := f.Create(t, fmt.Sprintf("%s-%d", prefix, i))
					requests[i] = tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: item.WorkItemID, MachineID: r.Binding.MachineID, SessionID: fmt.Sprintf("%s-%d", prefix, i), TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				}
				start := make(chan struct{})
				results := make(chan claimed, len(requests))
				var workers sync.WaitGroup
				for _, request := range requests {
					workers.Go(func() {
						<-start
						response := first.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, request)
						if response.Code == http.StatusConflict {
							return
						}
						hubfixture.RequireStatus(t, response, http.StatusOK)
						var lease tracker.NativeLease
						hubfixture.Decode(t, response, &lease)
						results <- claimed{claim: request, lease: lease, base: f.Base}
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
			if stored.Used != 4 || stored.ProjectUsed[first.Project.ID] != 4 {
				t.Fatalf("first slots=%+v", stored)
			}
			runtime := first.Hub.Request(t, http.MethodGet, first.Base+"/work-items/"+string(winners[0].claim.WorkItemID)+"/runtime", first.Token, nil)
			hubfixture.RequireStatus(t, runtime, http.StatusOK)
			var evidence tracker.NativeRuntimeEvidence
			hubfixture.Decode(t, runtime, &evidence)
			available := -1
			for _, capacity := range evidence.Capacity {
				if capacity.RunnerID == r.Binding.RunnerID {
					available = capacity.Available
				}
			}
			if available != 0 {
				t.Fatalf("runtime ignored remaining project headroom: available=%d", available)
			}
			change(r, 3, map[tracker.ProjectID]int{first.Project.ID: 3})
			claim(r, first, "lowered-project", http.StatusConflict)
			claim(r, other, "lowered-total", http.StatusConflict)
			for _, work := range winners {
				hubfixture.RequireStatus(t, first.Hub.Request(t, http.MethodPost, work.base+"/claims", r.Redemption.Credential, work.claim), http.StatusOK)
				hubfixture.RequireStatus(t, first.Hub.Request(t, http.MethodPost, work.base+"/leases/"+string(work.lease.ID)+"/renew", r.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: work.lease.FencingToken, TTLSeconds: 300}), http.StatusOK)
			}
			change(r, 8, map[tracker.ProjectID]int{first.Project.ID: 4})
			others := race(other, "other-race")
			claim(r, first, "ninth-first", http.StatusConflict)
			claim(r, other, "ninth-other", http.StatusConflict)
			if stored = read(r); stored.Used != 8 || stored.ProjectUsed[first.Project.ID] != 4 || stored.ProjectUsed[other.Project.ID] != 4 {
				t.Fatalf("combined slots=%+v", stored)
			}
			if shared {
				claim(second, other, "shared-host-ninth", http.StatusConflict)
			} else {
				independent := claim(second, first, "independent-host", http.StatusOK)
				release(second, independent)
			}
			change(r, 0, map[tracker.ProjectID]int{first.Project.ID: 0})
			hubfixture.RequireStatus(t, first.Hub.Request(t, http.MethodPost, winners[0].base+"/leases/"+string(winners[0].lease.ID)+"/renew", r.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: winners[0].lease.FencingToken, TTLSeconds: 300}), http.StatusOK)
			change(r, 8, nil)
			release(r, others[0])
			claim(r, first, "zero-project", http.StatusConflict)
			replacement := claim(r, other, "zero-project-other-starts", http.StatusOK)
			release(r, replacement)
			if err := first.Hub.Service.Close(); err != nil {
				t.Fatal(err)
			}
			first.Hub = first.Hub.Reopen(t)
			other.Hub = first.Hub
			if stored = read(r); stored.ProjectCapacityLimits[first.Project.ID] != 0 || stored.ProjectUsed[first.Project.ID] != 4 {
				t.Fatalf("restart lost limits or usage=%+v", stored)
			}
			siblingStored := read(second)
			if siblingStored.ProjectCapacityLimits[first.Project.ID] != 1 || siblingStored.ProjectCapacityLimits[other.Project.ID] != 2 {
				t.Fatalf("runner limits crossed identities=%+v", siblingStored)
			}
			expired := winners[0]
			result, err := first.Hub.Admin.ExecContext(t.Context(), "UPDATE leases SET acquired_at=acquired_at+($1::timestamptz-expires_at),renewed_at=renewed_at+($1::timestamptz-expires_at),expires_at=$1 WHERE organization_id=$2 AND lease_id=$3 AND fencing_token=$4 AND released_at IS NULL", now, first.Project.OrganizationID, expired.lease.ID, expired.lease.FencingToken)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				t.Fatalf("expired lease rows=%d error=%v", count, err)
			}
			renewal := tracker.NativeLeaseMutation{FencingToken: expired.lease.FencingToken, TTLSeconds: 300}
			renewPath := expired.base + "/leases/" + string(expired.lease.ID) + "/renew"
			refuseExpiredRenewal := func(path string, mutation tracker.NativeLeaseMutation) {
				t.Helper()
				refused := first.Hub.Request(t, http.MethodPost, path, r.Redemption.Credential, mutation)
				hubfixture.RequireStatus(t, refused, http.StatusConflict)
				var failure struct{ Code string }
				hubfixture.Decode(t, refused, &failure)
				if failure.Code != "stale_fencing_token" {
					t.Fatalf("expired lease renewal refusal=%q, want stale_fencing_token", failure.Code)
				}
			}
			refuseExpiredRenewal(renewPath, renewal)
			change(r, 8, map[tracker.ProjectID]int{})
			if stored = read(r); len(stored.ProjectCapacityLimits) != 0 || len(stored.ProjectIDs) != 2 {
				t.Fatalf("unsetting changed access=%+v", stored)
			}
			refuseExpiredRenewal(renewPath, renewal)
			claim(r, first, "inherited-total", http.StatusOK)
			claim(r, first, "expired-slot", http.StatusOK)
			claim(r, other, "inherited-ninth", http.StatusConflict)
			expired = winners[1]
			result, err = first.Hub.Admin.ExecContext(t.Context(), "UPDATE leases SET acquired_at=acquired_at+($1::timestamptz-expires_at),renewed_at=renewed_at+($1::timestamptz-expires_at),expires_at=$1 WHERE organization_id=$2 AND lease_id=$3 AND fencing_token=$4 AND released_at IS NULL", now, first.Project.OrganizationID, expired.lease.ID, expired.lease.FencingToken)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				t.Fatalf("expired lease rows=%d error=%v", count, err)
			}
			start := make(chan struct{})
			statuses := make(chan int, 2)
			var workers sync.WaitGroup
			for _, name := range []string{"expiry-race-first", "expiry-race-second"} {
				item := first.Create(t, name)
				contender := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: item.WorkItemID, MachineID: r.Binding.MachineID, SessionID: name, TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				workers.Go(func() {
					<-start
					statuses <- first.Hub.Request(t, http.MethodPost, first.Base+"/claims", r.Redemption.Credential, contender).Code
				})
			}
			close(start)
			workers.Wait()
			close(statuses)
			claimedSlots := 0
			for status := range statuses {
				if status == http.StatusOK {
					claimedSlots++
				} else if status != http.StatusConflict {
					t.Fatalf("expiry race status=%d", status)
				}
			}
			if claimedSlots != 1 || read(r).Used != 8 {
				t.Fatalf("claims after expiry raced beyond final slot: winners=%d", claimedSlots)
			}
			refuseExpiredRenewal(expired.base+"/leases/"+string(expired.lease.ID)+"/renew", tracker.NativeLeaseMutation{FencingToken: expired.lease.FencingToken, TTLSeconds: 300})

		})
	}
}

func TestRunnerProjectCapacityAdministration(t *testing.T) {
	f := hubfixture.NewNative(t, nil, "", "project-capacity-admin")
	other := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "other-capacity-admin")
	if err := f.Hub.Config.TenantRepository.WriteTenant(t.Context(), "org_foreign_capacity", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_foreign_capacity", Name: "Foreign organization", Slug: "foreign-capacity", CreatedAt: f.Hub.Config.Clock()})
	}); err != nil {
		t.Fatal(err)
	}
	foreign := hubfixture.NewNative(t, f.Hub, "org_foreign_capacity", "foreign-capacity-admin")
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.Enroll(t)
	base := map[string]any{"expected_revision": 1, "display_name": "Runner", "state": "active", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.Project.ID}, "idempotency_key": "limits"}
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
		{name: "unauthorized project", project: other.Project.ID, limit: 4, want: 422},
		{name: "cross organization project", project: foreign.Project.ID, limit: 4, want: 422},
		{name: "missing idempotency", key: " ", limit: 4, want: 422},
		{name: "worker", token: r.Redemption.Credential, limit: 4, want: 403},
		{name: "cross organization runner", path: "/api/v2/organizations/" + string(foreign.Project.OrganizationID) + "/runners/" + r.Binding.RunnerID + "/routing", limit: 4, want: 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := maps.Clone(base)
			project := test.project
			if project == "" {
				project = f.Project.ID
			}
			request["project_capacity_limits"] = map[tracker.ProjectID]any{project: test.limit}
			if test.key != "" {
				request["idempotency_key"] = test.key
			}
			token := test.token
			if token == "" {
				token = hubfixture.AdminToken
			}
			path := test.path
			if path == "" {
				path = r.IdentityPath() + "/routing"
			}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, path, token, request), test.want)
		})
	}
	base["project_ids"] = []tracker.ProjectID{f.Project.ID, other.Project.ID}
	base["project_capacity_limits"] = map[tracker.ProjectID]int{f.Project.ID: 0, other.Project.ID: 10000}
	response := f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, base)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var stored runnerauth.Runner
	hubfixture.Decode(t, response, &stored)
	if stored.Revision != 2 || stored.ProjectCapacityLimits[f.Project.ID] != 0 || stored.ProjectCapacityLimits[other.Project.ID] != 10000 || stored.State != "active" || stored.CapacityLimit != 2 {
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
			removal["project_ids"] = []tracker.ProjectID{f.Project.ID}
			delete(removal, "project_capacity_limits")
			if test.include {
				removal["project_capacity_limits"] = test.limits
			}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, removal), http.StatusUnprocessableEntity)
			response := f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", hubfixture.AdminToken, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var retained runnerauth.Runner
			hubfixture.Decode(t, response, &retained)
			if retained.Revision != stored.Revision || !maps.Equal(retained.ProjectCapacityLimits, stored.ProjectCapacityLimits) || len(retained.ProjectIDs) != 2 {
				t.Fatalf("refused access removal mutated routing=%+v", retained)
			}
		})
	}
	retry := f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, base)
	hubfixture.RequireStatus(t, retry, http.StatusOK)
	if !bytes.Equal(bytes.TrimSpace(response.Body.Bytes()), bytes.TrimSpace(retry.Body.Bytes())) {
		t.Fatal("routing retry changed its receipt")
	}
	changed := maps.Clone(base)
	changed["project_capacity_limits"] = map[tracker.ProjectID]int{}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, changed), http.StatusConflict)
	changed["idempotency_key"] = "stale-revision"
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, changed), http.StatusConflict)
	capacityResponse := f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/capacity", hubfixture.AdminToken, nil)
	hubfixture.RequireStatus(t, capacityResponse, http.StatusOK)
	var capacity runnerauth.CapacityView
	hubfixture.Decode(t, capacityResponse, &capacity)
	zero, unknown := capacity.Projects[f.Project.ID], capacity.Projects[other.Project.ID]
	if capacity.Status != "unknown" || capacity.Effective != nil || zero.Effective == nil || *zero.Effective != 0 || zero.Applied == nil || *zero.Applied != 0 || unknown.Effective != nil {
		t.Fatalf("project override forged effective total evidence=%+v", capacity)
	}
	heartbeat := f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{"display_name": "Runner", "capacity": 2, "version": "test"})
	hubfixture.RequireStatus(t, heartbeat, http.StatusOK)
	var snapshot runnerauth.RoutingSnapshot
	hubfixture.Decode(t, heartbeat, &snapshot)
	if !maps.Equal(snapshot.Routing.ProjectCapacityLimits, stored.ProjectCapacityLimits) {
		t.Fatal("heartbeat lost project overrides")
	}
	changed["expected_revision"] = 2
	delete(changed, "project_capacity_limits")
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, changed), http.StatusOK)
	read := f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", hubfixture.AdminToken, nil)
	hubfixture.RequireStatus(t, read, http.StatusOK)
	var after runnerauth.Runner
	hubfixture.Decode(t, read, &after)
	if !maps.Equal(after.ProjectCapacityLimits, stored.ProjectCapacityLimits) {
		t.Fatal("omitted overrides were cleared")
	}
	changed["expected_revision"] = after.Revision
	changed["idempotency_key"] = "clear-project-limits"
	changed["project_ids"] = []tracker.ProjectID{f.Project.ID}
	changed["project_capacity_limits"] = map[tracker.ProjectID]int{}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, changed), http.StatusOK)
	cleared := f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", hubfixture.AdminToken, nil)
	hubfixture.RequireStatus(t, cleared, http.StatusOK)
	var removed runnerauth.Runner
	hubfixture.Decode(t, cleared, &removed)
	if len(removed.ProjectCapacityLimits) != 0 || len(removed.ProjectIDs) != 1 || removed.ProjectIDs[0] != f.Project.ID {
		t.Fatalf("explicit override removal did not remove access atomically=%+v", removed)
	}
	delete(changed, "project_capacity_limits")
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, changed), http.StatusConflict)
	changed["expected_revision"] = removed.Revision
	changed["idempotency_key"] = "restore-project-access"
	changed["project_ids"] = []tracker.ProjectID{f.Project.ID, other.Project.ID}
	restored := f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, changed)
	hubfixture.RequireStatus(t, restored, http.StatusOK)
	var granted runnerauth.Runner
	hubfixture.Decode(t, restored, &granted)
	if len(granted.ProjectCapacityLimits) != 0 || len(granted.ProjectIDs) != 2 {
		t.Fatalf("re-added access revived stale ceilings=%+v", granted)
	}
}
