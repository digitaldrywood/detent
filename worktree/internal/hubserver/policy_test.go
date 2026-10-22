package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
<<<<<<< HEAD
	"github.com/digitaldrywood/detent/internal/config"
=======
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
>>>>>>> 03b850628 (fix: complete prj_6d4919bebd73446798e6cd807feda10e#1399)
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, "/api/v1/repositories/digitaldrywood/detent/policy", testHubAdminToken, map[string]string{"expected_policy_id": descriptor.ID}), http.StatusGone)
				}
			}
			response := performHubAPIRequest(t, service, http.MethodGet, "/api/v1/repositories/digitaldrywood/detent/policy", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var current struct {
				Policy *policy.Descriptor `json:"policy"`
			}
			decodeHubResponse(t, response, &current)
			if state != "legacy" {
				if current.Policy == nil || !reflect.DeepEqual(*current.Policy, hubTestPolicy()) {
					t.Fatalf("approved repository policy: %s", response.Body)
				}
			} else if current.Policy != nil || !strings.Contains(response.Body.String(), `"policy":null`) {
				t.Fatalf("absent repository policy: %s", response.Body)
			}
			wantRenew, wantEvent := http.StatusOK, http.StatusCreated
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
	current := performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
	var approval policy.Approval
	if current.Code == http.StatusOK {
		decodeHubResponse(t, current, &approval)
	}
	response := performHubAPIRequest(t, service, http.MethodPut, path, testHubAdminToken, policy.Change{ExpectedID: approval.Policy.ID, Policy: descriptor})
	requireNativeStatus(t, response, http.StatusOK)
}

func TestProjectPolicyAuthorizationAndAtomicClaims(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "policy")
	worker := f.worker(t, "worker")
	issue := f.create(t, "work")
	descriptor := hubTestPolicy()
	descriptor.Workflow = &policy.Workflow{Source: "detent.yaml", States: append(nativeFixtureStates(), tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Done"}})}
	descriptor = descriptor.WithID()
	claim := tracker.NativeClaim{WorkItemID: issue.WorkItemID, MachineID: "machine_abc", SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": claim.MachineID, "hostname": "runner", "capacity": 2, "version": "test"}), http.StatusOK)
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	runner.enroll(t)
	for _, token := range []string{worker, f.token, runner.redemption.Credential} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", token, policy.Change{Policy: descriptor}), http.StatusForbidden)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim), http.StatusUnprocessableEntity)
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	for _, field := range []string{"states", "workflow_markdown"} {
		request := map[string]any{"idempotency_key": "repository-override-" + field, "expected_revision": "2", "intake": "disabled", "projection": "disabled"}
		if field == "states" {
			request[field] = nativeFixtureStates()
		} else {
			request[field] = "---\ntracker:\n  kind: hub_native\n---\nWork\n"
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, request), http.StatusUnprocessableEntity)
	}
	for _, test := range []struct{ name, id string }{{"missing", ""}, {"unknown", "policy_" + strings.Repeat("b", 64)}} {
		t.Run(test.name, func(t *testing.T) {
			claim.PolicyID = test.id
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim), http.StatusUnprocessableEntity)
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
	event := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusOK)
	changed := descriptor
	changed.Gates.AutoPromote = true
	changed.Workflow = &policy.Workflow{Source: "detent.yaml", States: append(append([]tracker.NativeState(nil), descriptor.Workflow.States...), tracker.NativeState{Name: "QA"})}
	changed = changed.WithID()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptor.ID, Policy: changed}), http.StatusOK)
	pinned, err := f.service.database.leasePolicyID(t.Context(), lease.ID)
	if err != nil || pinned != descriptor.ID {
		t.Fatalf("approval changed existing lease policy: %s %v", pinned, err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
	reported := descriptor
	reported.SourceDigest = policy.Digest([]byte("reported configuration"))
	reported = reported.WithID()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", worker, policy.Observation{Descriptor: reported}), http.StatusNoContent)
	claim.PolicyID = reported.ID
	newIssue := f.create(t, "new policy work")
	claim.WorkItemID, claim.SessionID = newIssue.WorkItemID, "new-policy-session"
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var newLease tracker.NativeLease
	decodeHubResponse(t, response, &newLease)
	if newLease.PolicyID != reported.ID || newLease.ID == lease.ID {
		t.Fatalf("new claim lost reported configuration: %+v", newLease)
	}
	event.Type, event.Data.Sequence, event.Data.Handoff = "run.checkpointed", 2, nativeTestCheckpoint()
	event.IdempotencyKey, event.Data.PolicyID = "forged", changed.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusConflict)
	event.Data.PolicyID, event.IdempotencyKey = descriptor.ID, "pinned"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/policy", worker, map[string]string{"expected_policy_id": changed.ID}), http.StatusForbidden)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/policy", testHubAdminToken, map[string]string{"expected_policy_id": changed.ID}), http.StatusGone)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(newLease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: newLease.FencingToken, Reason: "cancelled"}), http.StatusNoContent)
	claim.WorkItemID = f.create(t, "reported configuration without approval").WorkItemID
	claim.SessionID = "reported-without-approval"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim), http.StatusOK)
	event.Data.PolicyID = descriptor.ID
	event.IdempotencyKey = "revoked"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "cancelled"}), http.StatusNoContent)
}

func TestConfigurationChangesPreserveLandingWork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		stale         int
		target        string
		refusal       string
		status        int
		barrier       bool
		activeLanding bool
		mutation      string
	}{
		{name: "zero", target: "Rework", status: http.StatusOK},
		{name: "one", stale: 1, target: "Rework", status: http.StatusOK},
		{name: "several", stale: 3, target: "Rework", status: http.StatusOK},
		{name: "fallback", stale: 2, target: "In Progress", status: http.StatusOK},
		{name: "rolling barrier", stale: 2, target: "Rework", status: http.StatusOK, barrier: true},
		{name: "active landing lease", stale: 1, target: "Rework", status: http.StatusOK, activeLanding: true},
		{name: "no refusal lane needed", stale: 2, target: "Done", status: http.StatusOK},
		{name: "unauthorized", stale: 2, target: "Rework", refusal: "unauthorized", status: http.StatusForbidden},
		{name: "stale approval", stale: 2, target: "Rework", refusal: "expected", status: http.StatusConflict},
		{name: "configuration does not transition issues", stale: 2, target: "Rework", refusal: "write", status: http.StatusOK},
		{name: "review policy replacement", stale: 3, target: "Rework", mutation: "review", status: http.StatusOK},
		{name: "review policy unauthorized", stale: 2, target: "Rework", mutation: "review", refusal: "unauthorized", status: http.StatusForbidden},
		{name: "review policy transition rollback", stale: 2, target: "Rework", mutation: "review", refusal: "write", status: http.StatusInternalServerError},
		{name: "repository policy revocation", stale: 3, target: "Rework", mutation: "revoke", status: http.StatusGone},
		{name: "repository policy revocation unauthorized", stale: 2, target: "Rework", mutation: "revoke", refusal: "unauthorized", status: http.StatusForbidden},
		{name: "repository revocation does not transition issues", stale: 2, target: "Rework", mutation: "revoke", refusal: "write", status: http.StatusGone},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "policy-landing")
			states := HostedProjectStates()
			states = append(states, tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Merging", "Done"}})
			for i := range states {
				if states[i].Name == "Merging" {
					states[i].Transitions = []string{test.target}
				}
			}
			previous := hubTestPolicy()
			previous.Workflow = &policy.Workflow{Source: "detent.yaml", States: states}
			previous = previous.WithID()
			next := previous
			next.Gates.AutoPromote = true
			if test.barrier {
				next.Gates.LandingMode = "rolling_barrier"
				next.Gates.LandingCommandDigest = policy.Digest([]byte("make check-barrier"))
			}
			next = next.WithID()
			controlPolicy := next
			if test.mutation != "" {
				controlPolicy = previous
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", controlPolicy)
			nextRules := defaultChangeReviewPolicy(previous)
			nextRules.RequireReview = true
			nextRules.ID = changerequest.PolicyID(nextRules)
			if test.mutation == "review" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "future-review"}, Policy: nextRules}), http.StatusOK)
			}
			createChange := func(name, state string, descriptor policy.Descriptor) changeFixture {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: name}, Title: name, State: state})
				requireNativeStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
				response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: name})
				requireNativeStatus(t, response, http.StatusOK)
				var change tracker.ChangeRequest
				decodeHubResponse(t, response, &change)
				path += "/" + change.ID
				input := changeTestInput()
				input.PolicyID = descriptor.ID
				bundle := changeTestSource(t, &input)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish"}, ChangeVersionInput: input, SourceBundle: bundle}), http.StatusOK)
				return changeFixture{nativeFixture: f, issue: issue, change: change, path: path}
			}
			current := createChange("current policy", "In Progress", controlPolicy)
			if test.mutation == "review" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "old-review"}, ExpectedID: nextRules.ID, Policy: defaultChangeReviewPolicy(previous)}), http.StatusOK)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: controlPolicy.ID, Policy: previous}), http.StatusOK)
			moveToMerging := func(change changeFixture) {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(change.issue.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "merging"}, ExpectedRevision: change.issue.Revision, State: "Merging", Reason: "user_requested"}), http.StatusOK)
			}
			moveToMerging(current)
			changes := []changeFixture{current}
			for i := range test.stale {
				change := createChange(fmt.Sprintf("stale %d", i), "In Progress", previous)
				moveToMerging(change)
				changes = append(changes, change)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			var heldAttempts []tracker.NativeAttempt
			for _, hold := range []struct {
				name        string
				disposition tracker.NativeDisposition
			}{
				{name: "human hold", disposition: tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
				{name: "permission hold", disposition: tracker.NativeDisposition{Status: "blocked", ReasonCode: "permission_wait"}},
			} {
				held := createChange(hold.name, "In Progress", previous)
				worker := f.worker(t, hold.name)
				lease := claimNativeAttempt(t, f, worker, hold.name, hold.name, held.issue.WorkItemID, previous.ID)
				event := nativeStartedEvent(lease)
				path := f.base + "/work-items/" + string(held.issue.WorkItemID)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.finished", "finished", 2
				event.Data.Outcome, event.Data.Disposition = "succeeded", &hold.disposition
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(held.issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "hold"}, ExpectedRevision: issue.Revision, State: "Blocked", Reason: "user_requested"}), http.StatusOK)
				attempt, err := readNativeAttempt(t.Context(), f.service.database.db, scope, string(held.issue.WorkItemID), event.Data.AttemptID, f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				heldAttempts = append(heldAttempts, attempt)
				changes = append(changes, held)
			}
			var active tracker.NativeIssue
			if test.activeLanding {
				active = changes[1].issue
			} else {
				active = f.create(t, "active execution")
			}
			worker := f.worker(t, "active-worker")
			lease := claimNativeAttempt(t, f, worker, "active-machine", "active-session", active.WorkItemID, previous.ID)
			before := make([]tracker.ChangeDetail, len(changes))
			issuesBefore := make([]tracker.NativeIssue, len(changes))
			for i, change := range changes {
				before[i] = change.detail(t)
				issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(change.issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				issuesBefore[i] = issue
			}
			token, expected := testHubAdminToken, previous.ID
			switch test.refusal {
			case "unauthorized":
				token = worker
			case "expected":
				expected = "policy_stale"
			case "write":
				statement := fmt.Sprintf("CREATE TRIGGER refuse_later_transition BEFORE UPDATE OF workflow_state_id ON issues WHEN OLD.native_id = '%s' BEGIN SELECT RAISE(ABORT, 'fixture transition failure'); END", changes[2].issue.WorkItemID)
				if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			drainTestNativeBoard(t, f.service)
			var sequenceBefore, floorBefore int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence,replay_floor FROM native_board_sequences WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&sequenceBefore, &floorBefore); err != nil {
				t.Fatal(err)
			}
			succeeded := test.status >= 200 && test.status < 300
			switch test.mutation {
			case "review":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", token, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "replace-review"}, ExpectedID: defaultChangeReviewPolicy(previous).ID, Policy: nextRules}), test.status)
			case "revoke":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/policy", token, map[string]string{"expected_policy_id": expected}), test.status)
			default:
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", token, policy.Change{ExpectedID: expected, Policy: next}), test.status)
			}
			if !succeeded {
				var sequenceAfter, floorAfter int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence,replay_floor FROM native_board_sequences WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&sequenceAfter, &floorAfter); err != nil {
					t.Fatal(err)
				}
				if sequenceAfter != sequenceBefore || floorAfter != floorBefore {
					t.Fatalf("failed approval reset board replay: (%d,%d) -> (%d,%d)", sequenceBefore, floorBefore, sequenceAfter, floorAfter)
				}
			}
			wantPolicy := previous.ID
			if succeeded && test.mutation == "" {
				wantPolicy = next.ID
			}
			approval, _, err := readProjectPolicy(t.Context(), f.service.database.db, string(scope.organization)+"/"+string(scope.project))
			if err != nil || approval.Policy.ID != wantPolicy {
				t.Fatalf("configuration = %+v, error = %v, want %s", approval, err, wantPolicy)
			}

			if test.mutation == "review" {
				rules, err := readChangePolicy(t.Context(), f.service.database.db, scope)
				wantRules := defaultChangeReviewPolicy(previous).ID
				if succeeded {
					wantRules = nextRules.ID
				}
				if err != nil || rules.ID != wantRules {
					t.Fatalf("review policy = %+v, error = %v, want %s", rules, err, wantRules)
				}
			}
			if test.barrier {
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				var id tracker.WorkItemID
				if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id = ?", current.issue.WorkItemID).Scan(&id); err != nil {
					t.Fatal(err)
				}
				ready, evaluated, _, err := nativeLandingCandidateReady(t.Context(), tx, &scope, id, lease.MachineID, 0, f.service.config.now())
				if err != nil || ready || !evaluated {
					t.Fatalf("stale landing eligibility = %t, evaluated = %t, error = %v", ready, evaluated, err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			for i, change := range changes {
				issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(change.issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				want := "Merging"
				if i > test.stale {
					want = "Blocked"
				} else if succeeded && test.mutation == "review" && i > 0 {
					want = test.target
				}
				if issue.State != want {
					t.Errorf("%s state = %s, want %s", issue.Title, issue.State, want)
				}
				if (test.mutation != "review" || i == 0 || i > test.stale || !succeeded) && !reflect.DeepEqual(issue, issuesBefore[i]) {
					t.Errorf("%s approval changed unrelated or held work", issue.Title)
				}
				if i > test.stale {
					prior := heldAttempts[i-test.stale-1]
					attempt, err := readNativeAttempt(t.Context(), f.service.database.db, scope, string(issue.WorkItemID), prior.AttemptID, f.service.config.now())
					if err != nil || !reflect.DeepEqual(attempt.Disposition, prior.Disposition) {
						t.Errorf("%s hold changed: %+v, error = %v", issue.Title, attempt.Disposition, err)
					}
				}
				after := change.detail(t)
				if !reflect.DeepEqual(after.Change, before[i].Change) || !reflect.DeepEqual(after.Versions, before[i].Versions) {
					t.Errorf("%s immutable change or version changed", issue.Title)
				}
				var transitions int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id = ? AND type = 'workflow.transitioned' AND json_extract(data_json, '$.reason') = 'stale_policy'", issue.WorkItemID).Scan(&transitions); err != nil {
					t.Fatal(err)
				}
				wantTransitions := 0
				if i <= test.stale && succeeded && test.mutation == "review" && i > 0 {
					wantTransitions = 1
				}
				if transitions != wantTransitions {
					t.Errorf("%s stale_policy transitions = %d, want %d", issue.Title, transitions, wantTransitions)
				}
			}
			pinned, err := f.service.database.leasePolicyID(t.Context(), lease.ID)
			if err != nil || pinned != previous.ID {
				t.Fatalf("active lease policy = %s, error = %v", pinned, err)
			}
			wantRenew := http.StatusOK
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), wantRenew)
		})
	}
}

func TestProjectPolicyIsolationAndRestart(t *testing.T) {
	t.Parallel()
	cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}
	service := openTestService(t, cfg)
	a := newNativeFixture(t, service, "", "human")
	b := newNativeFixture(t, service, a.project.OrganizationID, "automatic")
	human := hubTestPolicy()
	human.Gates.Kind, human.Gates.AutomatedReview = "human_review", ""
	human.Workflow = &policy.Workflow{Source: "WORKFLOW.md", States: append(a.project.States, tracker.NativeState{Name: "Plan Review"})}
	human = human.WithID()
	automatic := hubTestPolicy()
	automatic.Gates.AutoPromote, automatic.Gates.MergeMethod = true, "rebase"
	automatic.Workflow = &policy.Workflow{Source: "detent.yaml", States: append(b.project.States, tracker.NativeState{Name: "Repair", Dispatchable: true})}
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
			response = performHubAPIRequest(t, service, http.MethodGet, test.path, test.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var project tracker.NativeProject
			decodeHubResponse(t, response, &project)
			if !reflect.DeepEqual(project.States, test.want.Workflow.States) {
				t.Fatalf("reopened project workflow = %#v", project.States)
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

// TestProjectPolicyApprovalSeedsChangeReviewPolicy checks what approving a
// repository policy means for a native project's review expectation: a project
// with none gets the default, which asks for a person only under a
// human-review gate; the default follows the descriptor when it is approved
// again; an administrator's policy that pins checks is left to go stale; and a
// descriptor the default cannot satisfy seeds nothing.
func TestProjectPolicyApprovalSeedsChangeReviewPolicy(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "seeded")
	rulesPath := f.base + "/change-review-policy"
	read := func(t *testing.T, status int) tracker.ChangeReviewPolicy {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, rulesPath, f.token, nil)
		requireNativeStatus(t, response, status)
		var rules tracker.ChangeReviewPolicy
		if status == http.StatusOK {
			decodeHubResponse(t, response, &rules)
		}
		return rules
	}
	read(t, http.StatusNotFound)
	first := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", first)
	seeded := read(t, http.StatusOK)
	if first.Gates.Kind == "human_review" {
		t.Fatal("the fixture descriptor must not ask for human review")
	}
	if seeded.PolicyID != first.ID || seeded.RequireReview || len(seeded.RequiredChecks) != 0 || seeded.ID != changerequest.PolicyID(seeded) {
		t.Fatalf("seeded review policy = %#v", seeded)
	}
	second := first
	second.ConfigDigest = policy.Digest([]byte("second"))
	second = second.WithID()
	response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: first.ID, Policy: second})
	requireNativeStatus(t, response, http.StatusOK)
	if followed := read(t, http.StatusOK); followed.PolicyID != second.ID || followed.RequireReview || len(followed.RequiredChecks) != 0 {
		t.Fatalf("default review policy did not follow the descriptor: %#v", followed)
	}
	var principal string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name = 'operator-seeded'").Scan(&principal); err != nil {
		t.Fatal(err)
	}
	custom := tracker.ChangeReviewPolicy{PolicyID: second.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: principal, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}}
	response = performHubAPIRequest(t, f.service, http.MethodPut, rulesPath, testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "custom"}, ExpectedID: read(t, http.StatusOK).ID, Policy: custom})
	requireNativeStatus(t, response, http.StatusOK)
	third := second
	third.ConfigDigest = policy.Digest([]byte("third"))
	third = third.WithID()
	response = performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: second.ID, Policy: third})
	requireNativeStatus(t, response, http.StatusOK)
	if kept := read(t, http.StatusOK); kept.PolicyID != second.ID || len(kept.RequiredChecks) != 1 {
		t.Fatalf("an administrator's review policy was rewritten: %#v", kept)
	}

	checked := newNativeFixture(t, nil, "", "checked")
	descriptor := hubTestPolicy()
	descriptor.Gates.RequiredChecks = 1
	descriptor = descriptor.WithID()
	approveHubTestPolicy(t, checked.service, checked.base+"/policy", descriptor)
	requireNativeStatus(t, performHubAPIRequest(t, checked.service, http.MethodGet, checked.base+"/change-review-policy", checked.token, nil), http.StatusNotFound)

	reviewed := newNativeFixture(t, nil, "", "reviewed")
	gated := hubTestPolicy()
	gated.Gates.Kind, gated.Gates.AutomatedReview = "human_review", ""
	gated.Gates.HumanReview = true
	gated = gated.WithID()
	approveHubTestPolicy(t, reviewed.service, reviewed.base+"/policy", gated)
	response = performHubAPIRequest(t, reviewed.service, http.MethodGet, reviewed.base+"/change-review-policy", reviewed.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var asked tracker.ChangeReviewPolicy
	decodeHubResponse(t, response, &asked)
	if asked.PolicyID != gated.ID || !asked.RequireReview {
		t.Fatalf("a human-review gate seeded %#v, want a review requirement", asked)
	}
	for _, humanReview := range []bool{false, true} {
		t.Run(fmt.Sprintf("validator human review=%t", humanReview), func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "validator")
			descriptor := hubTestPolicy()
			descriptor.Gates.Validator, descriptor.Gates.HumanReview = true, humanReview
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/change-review-policy", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var rules tracker.ChangeReviewPolicy
			decodeHubResponse(t, response, &rules)
			if rules.PolicyID != descriptor.ID || rules.RequireReview != humanReview || len(rules.RequiredChecks) != 0 {
				t.Fatalf("validator default policy = %+v", rules)
			}
		})
	}

}

func seedNativeFixturePolicy(t *testing.T, s *Service, scope string) {
	t.Helper()
	seedNativeFixturePolicyContext(t.Context(), t, s, scope)
}

func seedNativeFixturePolicyContext(ctx context.Context, t *testing.T, s *Service, scope string) {
	t.Helper()
	descriptor := hubTestPolicy()
	raw, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.db.ExecContext(ctx, "INSERT INTO policy_revisions (scope, policy_id, metadata_json, approved_by, approved_at) VALUES (?, ?, ?, 'fixture', ?) ON CONFLICT DO NOTHING", scope, descriptor.ID, string(raw), formatHubTime(s.config.now())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.db.ExecContext(ctx, "INSERT INTO project_policies (scope, policy_id) VALUES (?, ?) ON CONFLICT DO NOTHING", scope, descriptor.ID); err != nil {
		t.Fatal(err)
	}
}

func clearNativeFixturePolicy(t *testing.T, f nativeFixture) {
	t.Helper()
	scope := string(f.project.OrganizationID) + "/" + string(f.project.ID)
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope=?", scope); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM policy_revisions WHERE scope=? AND approved_by='fixture'", scope); err != nil {
		t.Fatal(err)
	}
}

func TestApprovedPolicyClaimExposure(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.BillingModeMetered, config.BillingModeSubscription} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			cfg := config.Default()
			cfg.Tracker.Kind = config.TrackerHubNative
			cfg.Budget.BillingMode = mode
			cfg.Plan.Enabled = false
			cfg.Gate.SecurityAudit.Enabled = false
			cfg.Gate.Validator.Enabled = true
			cfg.Runners = config.Runners{Profile: "approved", Profiles: map[string]policy.Requirements{"approved": {RequiredTags: []string{"linux"}}}}
			descriptor, err := config.ResolvePolicy(config.Workflow{Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			var behavior map[string]json.RawMessage
			if err := json.Unmarshal(descriptor.Configuration.Behavior, &behavior); err != nil {
				t.Fatal(err)
			}
			behavior["RetiredBehavior"] = json.RawMessage(`{"Enabled":true}`)
			delete(behavior, "Gate")
			descriptor.Configuration.Behavior, err = json.Marshal(behavior)
			if err != nil {
				t.Fatal(err)
			}
			descriptor.SourceRevision = strings.Repeat("b", 64)
			descriptor = descriptor.WithID()
			raw, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			db := f.service.database.db
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			policyScope := string(scope.organization) + "/" + string(scope.project)
			if _, err := db.ExecContext(t.Context(), "INSERT INTO policy_revisions (scope,policy_id,metadata_json,approved_by,approved_at) VALUES (?,?,?,'fixture',?)", policyScope, descriptor.ID, string(raw), formatHubTime(f.service.config.now())); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), "UPDATE project_policies SET policy_id=? WHERE scope=?", descriptor.ID, policyScope); err != nil {
				t.Fatal(err)
			}
			r := placementFixtureRunner(t, f, "local-runner", "local", false, 1, "gpt-6.1-sol")
			exposure, err := monthlyClaimExposure(t.Context(), db, scope, r.binding.MachineID, false)
			if err != nil || exposure.SpriteInfrastructure || exposure.RunnerAPI != (mode == config.BillingModeMetered) {
				t.Fatalf("claim exposure = %+v, error = %v", exposure, err)
			}
			contract, err := nativeIssueContractConfig(t.Context(), db, scope)
			if err != nil || contract.Budget.EffectiveBillingMode() != mode || contract.Plan.Enabled || contract.Gate.SecurityAudit.Enabled != descriptor.Gates.SecurityAudit || contract.Gate.Validator.Enabled != descriptor.Gates.Validator {
				t.Fatalf("issue contract configuration = %+v, error = %v", contract, err)
			}
			snapshot, err := readPlacementSnapshotWithCapacity(t.Context(), db, scope, f.service.config.now(), nil, newPlacementCapacity(), make(map[string]bool))
			if err != nil || snapshot.resolver == nil || !reflect.DeepEqual(snapshot.Requirements, policy.Requirements{RequiredTags: []string{"linux"}}) || strings.Contains(strings.ToLower(snapshot.Reason), "approval") {
				t.Fatalf("placement = %+v, error = %v", snapshot, err)
			}
			approved, found, err := readProjectPolicy(t.Context(), db, policyScope)
			if err != nil || !found || !reflect.DeepEqual(approved.Policy, descriptor) {
				t.Fatalf("stored policy changed: %+v, error = %v", approved, err)
			}
		})
	}
}

func TestNativePolicySubmissionConsistency(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*policy.Descriptor)
	}{
		{"gates", func(d *policy.Descriptor) { d.Gates.SecurityAudit = !d.Gates.SecurityAudit }},
		{"validator", func(d *policy.Descriptor) { d.Gates.Validator = !d.Gates.Validator }},
		{"planning", func(d *policy.Descriptor) { d.Gates.PlanEnabled = !d.Gates.PlanEnabled }},
		{"requirements", func(d *policy.Descriptor) { d.Requirements.RequiredTags = []string{"privileged"} }},
		{"profile", func(d *policy.Descriptor) { d.Profile = "privileged" }},
		{"revision", func(d *policy.Descriptor) { d.SourceRevision = strings.Repeat("b", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			path := string(f.project.OrganizationID) + "/" + string(f.project.ID)
			before, found, err := readProjectPolicy(t.Context(), f.service.database.db, path)
			if err != nil || !found {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.Tracker.Kind = config.TrackerHubNative
			descriptor, err := config.ResolvePolicy(config.Workflow{Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			test.change(&descriptor)
			descriptor = descriptor.WithID()
			if err := descriptor.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, submission := range []struct {
				method, path string
				body         any
			}{
				{http.MethodPut, f.base + "/policy", policy.Change{ExpectedID: before.Policy.ID, Policy: descriptor}},
				{http.MethodPost, f.base + "/policy/observed", policy.Observation{Descriptor: descriptor}},
			} {
				response := performHubAPIRequest(t, f.service, submission.method, submission.path, testHubAdminToken, submission.body)
				requireNativeStatus(t, response, http.StatusUnprocessableEntity)
				if !strings.Contains(response.Body.String(), "policy_mismatch") {
					t.Fatalf("submission refused for unrelated reason: %s", response.Body.String())
				}
			}
			after, found, err := readProjectPolicy(t.Context(), f.service.database.db, path)
			if err != nil || !found || !reflect.DeepEqual(before, after) {
				t.Fatalf("inconsistent submission changed approval: %+v, error = %v", after, err)
			}
			var observations int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM project_observed_policies WHERE scope=?", path).Scan(&observations); err != nil {
				t.Fatal(err)
			}
			if observations != 0 {
				t.Fatalf("inconsistent observations stored: %d", observations)
			}
		})
	}
}

func TestProjectPolicyReadsWithoutApproval(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		approved bool
		large    bool
	}{
		{name: "no approval"},
		{name: "approved", approved: true},
		{name: "no approval with large observation", large: true},
		{name: "approved with large observation", approved: true, large: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			approved := test.approved
			t.Parallel()
			f := newNativeFixture(t, nil, "", "policy-read")
			scope := string(f.project.OrganizationID) + "/" + string(f.project.ID)
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope=?", scope); err != nil {
				t.Fatal(err)
			}
			descriptor := hubTestPolicy()
			if approved {
				approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			}
			candidate := descriptor
			candidate.Gates.AutoPromote = true
			if test.large {
				workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
					ConfigPath: "detent.yaml", HasConfig: true,
					Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\ngate:\n  run: true\n  required_status_checks: []\n"),
					WorkflowPath: "WORKFLOW.md", Workflow: []byte(strings.Repeat("Implement the assigned issue.\n", operatortool.MaxResultBytes/28)),
				})
				if err != nil {
					t.Fatal(err)
				}
				candidate, err = workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
			}
			candidate = candidate.WithID()
			worker := f.worker(t, "policy-reader")
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", worker, policy.Observation{Descriptor: candidate}), http.StatusNoContent)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "policy_read_machine", "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
			_, err := monthlyClaimExposure(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, "policy_read_machine", false)
			if approved && err != nil {
				t.Fatal(err)
			}
			if !approved {
				var refusal *nativeError
				if !errors.As(err, &refusal) || refusal.Code != "policy_not_approved" {
					t.Fatalf("unapproved claim exposure: %v", err)
				}
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var rest struct {
				Policy     *policy.Descriptor      `json:"policy"`
				ApprovedBy string                  `json:"approved_by"`
				ApprovedAt string                  `json:"approved_at"`
				Observed   []policy.ObservedPolicy `json:"observed_policies"`
				History    []policy.WorkflowApply  `json:"history"`
			}
			decodeHubResponse(t, response, &rest)
			if approved {
				if rest.Policy == nil || !reflect.DeepEqual(*rest.Policy, descriptor) || rest.ApprovedBy == "" || rest.ApprovedAt == "" {
					t.Fatalf("approved payload changed: %s", response.Body)
				}
			} else if rest.Policy != nil || !strings.Contains(response.Body.String(), `"policy":null`) || rest.ApprovedBy != "" || rest.ApprovedAt != "" || len(rest.History) != 0 {
				t.Fatalf("missing approval: %s", response.Body)
			}
			if len(rest.Observed) != 1 || !reflect.DeepEqual(rest.Observed[0].Policy, candidate) {
				t.Fatalf("observed candidates: %s", response.Body)
			}
			captured := make(chan context.Context, 1)
			f.service.echo.GET("/api/v2/organizations/:organization/policy-read-test", func(c echo.Context) error { captured <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/policy-read-test", testHubAdminToken, nil), http.StatusOK)
			ctx := operatortool.BindConnection(<-captured, "policy-read", "policy fixture")
			executor := hubProjectExecutor{f.service}
			if err := executor.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			for _, transport := range []string{"stdio", "http"} {
				call := hostedContextProtocol(t, f.service, ctx, transport)
				reply := call("tools/call", "get_project_policy", map[string]any{"project_id": string(f.project.ID)})
				var result struct {
					IsError    bool `json:"isError"`
					Structured struct {
						Data json.RawMessage `json:"data"`
					} `json:"structuredContent"`
				}
				if err := json.Unmarshal(reply.Result, &result); err != nil {
					t.Fatal(err)
				}
				if test.large {
					var data projectPolicyRead
					if result.IsError || json.Unmarshal(result.Structured.Data, &data) != nil || !data.ObservedPolicyDetailsOmitted || len(data.ObservedPolicies) != 1 || data.ObservedPolicies[0].Policy.ID != candidate.ID || len(result.Structured.Data) > operatortool.MaxResultBytes {
						t.Fatalf("%s large policy read failed: %s", transport, reply.Result)
					}
					if !reflect.DeepEqual(data.Policy, rest.Policy) || !reflect.DeepEqual(data.History, rest.History) {
						t.Fatalf("%s large observation changed approval or history", transport)
					}
					continue
				}
				var data any
				var expected any
				if result.IsError || json.Unmarshal(result.Structured.Data, &data) != nil || json.Unmarshal(response.Body.Bytes(), &expected) != nil || !reflect.DeepEqual(data, expected) {
					t.Fatalf("%s policy differs from REST: %s; REST %s", transport, reply.Result, response.Body)
				}
			}
		})
	}
}
