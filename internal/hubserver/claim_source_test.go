package hubserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestClaimSourceAvailabilityFollowsCurrentSource(t *testing.T) {
	t.Parallel()
	f := placementNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	holder := placementFixtureRunner(t, f, "source-runner", "source", false, 2, "gpt-6.1-sol", "test-model")
	other := placementFixtureRunner(t, f, "other-runner", "other", false, 2, "gpt-6.1-sol", "test-model")
	issue := f.create(t, "source pinned to its runner")
	item := f.base + "/work-items/" + string(issue.WorkItemID)
	current := func() tracker.NativeIssue {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, item, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var value tracker.NativeIssue
		decodeHubResponse(t, response, &value)
		return value
	}
	claim := func(r runnerFixture, session string) *httptest.ResponseRecorder {
		t.Helper()
		return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, current(), session))
	}
	release := func(r runnerFixture, lease tracker.NativeLease) {
		t.Helper()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
	}
	claimed := func(r runnerFixture, session string) tracker.NativeLease {
		t.Helper()
		response := claim(r, session)
		requireNativeStatus(t, response, http.StatusOK)
		var lease tracker.NativeLease
		decodeHubResponse(t, response, &lease)
		return lease
	}

	lease := claimed(holder, "holder-first")
	event := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, item+"/events", holder.redemption.Credential, event), http.StatusOK)
	event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "holder-local-checkpoint", 2
	event.Data.Handoff = nativeTestCheckpoint()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, item+"/events", holder.redemption.Credential, event), http.StatusOK)
	response := performHubAPIRequest(t, f.service, http.MethodPost, item+"/changes", f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "source-change"}, Title: "Source pinned change"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	response = performHubAPIRequest(t, f.service, http.MethodPost, item+"/changes/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "unsourced-version"}, ChangeVersionInput: changeTestInput()})
	requireNativeStatus(t, response, http.StatusOK)
	var unsourced tracker.ChangeVersion
	decodeHubResponse(t, response, &unsourced)
	release(holder, lease)

	want := fmt.Sprintf("Change %s version %s has no verified Hub source; recapture the exact head on source runner %s", change.ID, unsourced.ID, holder.binding.MachineID)
	for range 2 {
		requireNativeCode(t, claim(other, "other-refused"), http.StatusConflict, "no_claimable_work")
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, item+"/runtime", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var evidence tracker.NativeRuntimeEvidence
	decodeHubResponse(t, response, &evidence)
	if evidence.LatestDecision == nil || evidence.LatestDecision.Data.Decision == nil {
		t.Fatalf("source refusal left no scheduler decision: %#v", evidence)
	}
	decision := evidence.LatestDecision.Data.Decision
	if decision.Source != "native_runner_routing" || decision.Outcome != "skipped" || decision.RunnerID != other.binding.RunnerID || decision.Reason != want {
		t.Fatalf("source refusal decision=%#v", decision)
	}
	if state := current().State; state != issue.State {
		t.Fatalf("source refusal moved the item to %q", state)
	}

	lease = claimed(holder, "holder-second")
	release(holder, lease)

	input := changeTestInput()
	input.HeadSHA = unsourced.HeadSHA
	response = performHubAPIRequest(t, f.service, http.MethodPost, item+"/changes/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "sourced-version"}, ExpectedVersionID: unsourced.ID, ChangeVersionInput: input, SourceBundle: changeTestSource(t, &input)})
	requireNativeStatus(t, response, http.StatusOK)
	lease = claimed(other, "other-after-source")
	if lease.MachineID != other.binding.MachineID {
		t.Fatalf("restored source claim lease=%+v", lease)
	}
}
