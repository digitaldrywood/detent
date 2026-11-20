package hubserver

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRoundVerdictComparesEvidenceAndCannotOverrideRefusal(t *testing.T) {
	t.Parallel()
	clock := &leaseTestClock{value: time.Date(2026, 11, 19, 21, 0, 0, 0, time.UTC)}
	f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Clock: clock.Now}), "", "Round verdicts")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "Keep the same acceptance criterion across repairs")
	worker := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	worker.enroll(t)
	lease := nativeAttemptFixtureLease(t, worker, issue, "round-owner")
	event := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker.redemption.Credential, event), http.StatusOK)

	const criterion = "Complete the requested work."
	tree := strings.Repeat("c", 40)
	input := progresscheck.Publication{
		Mutation:  tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken},
		AttemptID: event.Data.AttemptID, PolicyID: lease.PolicyID,
		RoundInput: progresscheck.RoundInput{Phase: "workspace_preparation", Action: "continue", NextPhase: "implement"},
	}
	first := input
	first.RoundID, first.IdempotencyKey = "setup before source", "setup before source"
	setup := performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", worker.redemption.Credential, first)
	requireNativeStatus(t, setup, http.StatusOK)

	previous := first.RoundID
	for _, round := range []struct {
		key       string
		head      string
		action    string
		command   *int
		forged    bool
		advanced  bool
		regressed bool
		want      int
	}{
		{key: "forged criterion", head: strings.Repeat("b", 40), action: "continue", forged: true, want: http.StatusOK},
		{key: "initial", head: strings.Repeat("a", 40), action: "continue", command: new(1), want: http.StatusOK},
		{key: "different commit same refusal", head: strings.Repeat("b", 40), action: "continue", command: new(1), want: http.StatusOK},
		{key: "unsupported completion", head: strings.Repeat("b", 40), action: "done", want: http.StatusUnprocessableEntity},
		{key: "verified advancement", head: strings.Repeat("d", 40), action: "continue", command: new(0), advanced: true, want: http.StatusOK},
		{key: "verified regression", head: strings.Repeat("e", 40), action: "continue", command: new(1), regressed: true, want: http.StatusOK},
		{key: "real question", head: strings.Repeat("e", 40), action: "human_action", want: http.StatusOK},
		{key: "machine park", head: strings.Repeat("e", 40), action: "park", want: http.StatusUnprocessableEntity},
	} {
		input.RoundID, input.IdempotencyKey = round.key, round.key
		input.Phase, input.HeadSHA, input.TreeSHA = "project_gate", round.head, tree
		input.Action, input.NextPhase, input.HumanAction = round.action, "implement", ""
		input.FailureSignature = "test:TestAcceptance"
		input.Criteria = []gate.CriterionEvidence{{Criterion: criterion, Kind: gate.EvidenceNotVerified, Reference: "TestAcceptance failed"}}
		if round.forged {
			input.Criteria[0] = gate.CriterionEvidence{Criterion: criterion, Kind: gate.EvidenceReceipt, Reference: "TestAcceptance", Behavior: "The requested output matches", ReceiptIDs: []string{"fabricated"}, HeadSHA: round.head, TreeSHA: tree}
		}
		if round.command != nil {
			event.Type = "run.observed"
			event.Data.Sequence++
			event.IdempotencyKey = "observe " + round.key
			validation := &gate.CommandResult{ReceiptID: "receipt_" + strings.Repeat(round.head[:1], 32), Stage: gate.StageSourceFinalization, Command: "go test -run TestAcceptance ./example", HeadSHA: round.head, TreeSHA: tree, ExitCode: *round.command, DurationNS: 1}
			event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: clock.Now(), Validation: validation}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker.redemption.Credential, event), http.StatusOK)
			input.Criteria[0].ReceiptIDs = []string{validation.ReceiptID}
			if *round.command == 0 {
				input.FailureSignature = ""
				input.Criteria[0] = gate.CriterionEvidence{Criterion: criterion, Kind: gate.EvidenceReceipt, Reference: validation.Command, Behavior: "The requested output matches", ReceiptIDs: []string{validation.ReceiptID}, HeadSHA: round.head, TreeSHA: tree}
			}
		}
		if round.action != "continue" {
			input.NextPhase = ""
		}
		if round.action == "human_action" {
			input.HumanAction = "Which external provider may this project use?"
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", worker.redemption.Credential, input)
		requireNativeStatus(t, response, round.want)
		if round.want == http.StatusOK {
			var receipt progresscheck.RoundVerdict
			decodeHubResponse(t, response, &receipt)
			if receipt.RoundID != round.key || receipt.PreviousRoundID != previous || receipt.Advanced != round.advanced ||
				receipt.Regressed != round.regressed || receipt.NextPhase != input.NextPhase ||
				receipt.HumanAction != input.HumanAction || receipt.FencingToken != lease.FencingToken ||
				(len(receipt.VerifiedCriteria) != 0) != round.advanced {
				t.Fatalf("round %q returned %+v", round.key, receipt)
			}
			previous = receipt.RoundID
		}
	}
	checkLatest := func(credential, itemPath, want string) {
		t.Helper()
		latest := performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"/progress?round=latest", credential, nil)
		requireNativeStatus(t, latest, http.StatusOK)
		var receipt progresscheck.RoundVerdict
		decodeHubResponse(t, latest, &receipt)
		if receipt.RoundID != want {
			t.Fatalf("latest round = %q, want %q", receipt.RoundID, want)
		}
	}

	retry := performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", worker.redemption.Credential, first)
	requireNativeStatus(t, retry, http.StatusOK)
	if retry.Body.String() != setup.Body.String() {
		t.Fatal("retry after a lost response returned a different verdict")
	}
	conflict := first
	conflict.Reason = "different model decision"
	requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", worker.redemption.Credential, conflict), http.StatusConflict, "idempotency_conflict")
	checkLatest(worker.redemption.Credential, path, previous)
	if item := readWorkItem(t, f, issue.WorkItemID, ""); item.State != issue.State {
		t.Fatalf("verdict changed workflow from %s to %s", issue.State, item.State)
	}

	clock.Advance(time.Hour)
	requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", worker.redemption.Credential, first), http.StatusConflict, "stale_execution")
	replacement := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate)
	replacement.enroll(t)
	next := nativeAttemptFixtureLease(t, replacement, issue, "round-replacement")
	nextEvent := nativeStartedEvent(next)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", replacement.redemption.Credential, nextEvent), http.StatusOK)
	checkLatest(replacement.redemption.Credential, path, previous)
	replacementRound := first
	replacementRound.RoundID, replacementRound.IdempotencyKey = "replacement round", "replacement round"
	replacementRound.AttemptID, replacementRound.LeaseID, replacementRound.FencingToken = nextEvent.Data.AttemptID, next.ID, next.FencingToken
	response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", replacement.redemption.Credential, replacementRound)
	requireNativeStatus(t, response, http.StatusOK)
	var carried progresscheck.RoundVerdict
	decodeHubResponse(t, response, &carried)
	if carried.PreviousRoundID != previous {
		t.Fatalf("replacement verdict lost previous round: %+v", carried)
	}

	other := newNativeFixture(t, f.service, "org_first_second", "Other round project")
	f.service.localOrganization = string(other.project.OrganizationID)
	approveHubTestPolicy(t, other.service, other.base+"/policy", hubTestPolicy())
	otherWorker := prepareRunner(t, other, runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate)
	otherWorker.enroll(t)
	otherIssue := other.create(t, "Independent round")
	otherPath := other.base + "/work-items/" + string(otherIssue.WorkItemID)
	foreign := otherPath + "/progress?round=" + url.QueryEscape(previous)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, foreign, otherWorker.redemption.Credential, nil), http.StatusNotFound)
	otherLease := nativeAttemptFixtureLease(t, otherWorker, otherIssue, "other-round")
	otherEvent := nativeStartedEvent(otherLease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, otherPath+"/events", otherWorker.redemption.Credential, otherEvent), http.StatusOK)
	otherRound := first
	otherRound.AttemptID, otherRound.LeaseID, otherRound.FencingToken = otherEvent.Data.AttemptID, otherLease.ID, otherLease.FencingToken
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, otherPath+"/progress", otherWorker.redemption.Credential, otherRound), http.StatusOK)
	checkLatest(otherWorker.redemption.Credential, otherPath, first.RoundID)
	f.service.localOrganization = string(f.project.OrganizationID)
	checkLatest(replacement.redemption.Credential, path, replacementRound.RoundID)
}
