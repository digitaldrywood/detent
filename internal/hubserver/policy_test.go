package hubserver

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCompatibilityPolicyUpgradeAndRevocation(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"legacy", "approved", "revoked"} {
		t.Run(state, func(t *testing.T) {
			service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
			_, issueID := seedProjection(t, service.database.db)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, "/api/v1/machines/register", testHubAdminToken, map[string]any{"id": "machine_abc", "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
			lease, err := service.database.Claim(t.Context(), tracker.ClaimRequest{WorkItemID: tracker.WorkItemID(issueID), MachineID: "machine_abc", SessionID: "session", TTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if state != "legacy" {
				descriptor := hubTestPolicy()
				approveHubTestPolicy(t, service, "/api/v1/repositories/digitaldrywood/detent/policy", descriptor)
				if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO lease_policies (lease_id, scope, policy_id) VALUES (?, ?, ?)", lease.ID, "repository:digitaldrywood/detent", descriptor.ID); err != nil {
					t.Fatal(err)
				}
				if state == "revoked" {
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, "/api/v1/repositories/digitaldrywood/detent/policy", testHubAdminToken, map[string]string{"expected_policy_id": descriptor.ID}), http.StatusNoContent)
				}
			}
			wantRenew, wantEvent := http.StatusConflict, http.StatusConflict
			if state == "approved" {
				wantRenew, wantEvent = http.StatusOK, http.StatusCreated
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, "/api/v1/leases/"+string(lease.ID)+"/renew", testHubAdminToken, map[string]any{"fencing_token": lease.FencingToken, "ttl_seconds": 90}), wantRenew)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, fmt.Sprintf("/api/v1/work-items/%d/events", issueID), testHubAdminToken, map[string]any{"fencing_token": lease.FencingToken, "kind": "progress", "payload": map[string]string{"step": "plan"}}), wantEvent)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, "/api/v1/leases/"+string(lease.ID)+"/release", testHubAdminToken, map[string]any{"fencing_token": lease.FencingToken, "reason": "cancelled"}), http.StatusNoContent)
		})
	}
}

func hubTestPolicy() policy.Descriptor {
	return policy.Descriptor{
		SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
		Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
	}.WithID()
}

func approveHubTestPolicy(t *testing.T, service *Service, path string, descriptor policy.Descriptor) {
	t.Helper()
	response := performHubAPIRequest(t, service, http.MethodPut, path, testHubAdminToken, policy.Change{Policy: descriptor})
	requireNativeStatus(t, response, http.StatusOK)
}

func TestProjectPolicyAuthorizationAndAtomicClaims(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "policy")
	worker := f.worker(t, "worker")
	issue := f.create(t, "work")
	descriptor := hubTestPolicy()
	claim := tracker.NativeClaim{WorkItemID: issue.WorkItemID, MachineID: "machine_abc", SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": claim.MachineID, "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
	for _, token := range []string{worker, f.token} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", token, policy.Change{Policy: descriptor}), http.StatusForbidden)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim), http.StatusConflict)
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	for _, test := range []struct{ name, id string }{{"missing", ""}, {"stale", "policy_" + strings.Repeat("b", 64)}} {
		t.Run(test.name, func(t *testing.T) {
			claim.PolicyID = test.id
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim), http.StatusConflict)
			var leases int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM leases").Scan(&leases); err != nil || leases != 0 {
				t.Fatalf("rejected claim allocated a lease: %d %v", leases, err)
			}
		})
	}
	claim.PolicyID = descriptor.ID
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.PolicyID != descriptor.ID {
		t.Fatalf("lease lost policy: %#v", lease)
	}
	changed := descriptor
	changed.Gates.AutoPromote = true
	changed = changed.WithID()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptor.ID, Policy: changed}), http.StatusConflict)
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "forged"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{RunID: newNativeID("run"), AttemptID: newNativeID("attempt"), PolicyID: changed.ID, LeaseID: lease.ID, FencingToken: lease.FencingToken}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusConflict)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/policy", worker, map[string]string{"expected_policy_id": descriptor.ID}), http.StatusForbidden)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/policy", testHubAdminToken, map[string]string{"expected_policy_id": descriptor.ID}), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusConflict)
	event.Data.PolicyID = descriptor.ID
	event.IdempotencyKey = "revoked"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusConflict)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "cancelled"}), http.StatusNoContent)
}

func TestProjectPolicyIsolationAndRestart(t *testing.T) {
	t.Parallel()
	cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}
	service := openTestService(t, cfg)
	a := newNativeFixture(t, service, "", "human")
	b := newNativeFixture(t, service, a.project.OrganizationID, "automatic")
	human := hubTestPolicy()
	human.Gates.Kind, human.Gates.AutomatedReview = "human_review", ""
	human = human.WithID()
	automatic := hubTestPolicy()
	automatic.Gates.AutoPromote, automatic.Gates.MergeMethod = true, "rebase"
	automatic = automatic.WithID()
	approveHubTestPolicy(t, service, a.base+"/policy", human)
	approveHubTestPolicy(t, service, b.base+"/policy", automatic)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, b.base+"/policy", a.token, nil), http.StatusNotFound)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service = openTestService(t, cfg)
	for _, test := range []struct {
		name, path, token string
		want              policy.Descriptor
	}{
		{"human", a.base, a.token, human}, {"automatic", b.base, b.token, automatic},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, service, http.MethodGet, test.path+"/policy", test.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var approval policy.Approval
			decodeHubResponse(t, response, &approval)
			if err := approval.Policy.Match(test.want); err != nil {
				t.Fatal(err)
			}
			if approval.ApprovedBy == "" || approval.ApprovedAt == "" {
				t.Fatalf("provenance missing: %#v", approval)
			}
		})
	}
}

func TestProjectPolicyConcurrentApproval(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "concurrent-policy")
	initial := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", initial)
	var wait sync.WaitGroup
	statuses := make(chan int, 2)
	for _, method := range []string{"merge", "rebase"} {
		wait.Go(func() {
			proposal := initial
			proposal.Gates.MergeMethod = method
			proposal = proposal.WithID()
			statuses <- performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: initial.ID, Policy: proposal}).Code
		})
	}
	wait.Wait()
	close(statuses)
	winners := 0
	for status := range statuses {
		if status == http.StatusOK {
			winners++
		} else if status != http.StatusConflict {
			t.Fatalf("unexpected status %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("approval winners = %d, want 1", winners)
	}
}

func TestPolicyRequirementsNeverEnrollOrGrantCapabilities(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		requirements policy.Requirements
	}{
		{"privileged tag", policy.Requirements{RequiredTags: []string{"production"}}},
		{"runner selector", policy.Requirements{RunnerID: "runner_privileged"}},
		{"machine selector", policy.Requirements{MachineID: "machine_other"}},
		{"forged matching machine", policy.Requirements{MachineID: "machine_abc"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "requirements")
			worker := f.worker(t, "worker")
			f.create(t, "work")
			descriptor := hubTestPolicy()
			descriptor.Requirements = test.requirements
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "machine_abc", "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: "machine_abc", SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim)
			requireNativeStatus(t, response, http.StatusConflict)
			if !strings.Contains(response.Body.String(), "selector_no_match") {
				t.Fatal(response.Body.String())
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM machines").Scan(&count); err != nil || count != 1 {
				t.Fatalf("requirements enrolled a machine: %d %v", count, err)
			}
		})
	}
}

func TestPolicyClaimChecksUseDatabaseTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, expires string
		status        int
	}{
		{"active offset", "2026-09-05T07:00:01-05:00", http.StatusConflict},
		{"exact expiry", "2026-09-05T07:00:00-05:00", http.StatusOK},
		{"expired fractional", "2026-09-05T11:59:59.9Z", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "expiry")
			worker := f.worker(t, "worker")
			f.create(t, "work")
			descriptor := hubTestPolicy()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "machine_abc", "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: "machine_abc", SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at = ? WHERE lease_id = ?", test.expires, lease.ID); err != nil {
				t.Fatal(err)
			}
			changed := descriptor
			changed.Gates.MergeMethod = "rebase"
			changed = changed.WithID()
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptor.ID, Policy: changed}), test.status)
			pinned, err := f.service.database.leasePolicyID(t.Context(), lease.ID)
			if err != nil || pinned != descriptor.ID {
				t.Fatalf("historical lease pin changed: %s %v", pinned, err)
			}
		})
	}
}
