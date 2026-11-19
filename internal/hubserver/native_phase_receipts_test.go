package hubserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type phaseReceiptFixture struct {
	tracker.Mutation
	Phase       string `json:"phase"`
	AttemptID   string `json:"attempt_id"`
	Branch      string `json:"branch"`
	HeadSHA     string `json:"head_sha"`
	TreeSHA     string `json:"tree_sha"`
	PolicyID    string `json:"policy_id"`
	InputDigest string `json:"input_digest"`
}

func TestNativePhaseReceiptsKeepSourceAcrossLeaseTransfer(t *testing.T) {
	t.Parallel()
	clock := &leaseTestClock{value: time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)}
	f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{Clock: clock.Now}), "", "Phase continuation")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "Retain phase source")
	path := f.base + "/work-items/" + string(issue.WorkItemID)

	owner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	owner.enroll(t)
	lease := nativeAttemptFixtureLease(t, owner, issue, "first-owner", 90*time.Minute)
	start := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", owner.redemption.Credential, start), http.StatusOK)

	clock.Advance(45 * time.Minute)
	renewal := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 5400})
	requireNativeStatus(t, renewal, http.StatusOK)
	var renewed tracker.NativeLease
	decodeHubResponse(t, renewal, &renewed)
	if renewed.ID != lease.ID || renewed.FencingToken != lease.FencingToken || !renewed.ExpiresAt.Equal(clock.Now().Add(90*time.Minute)) {
		t.Fatalf("renewal after outage changed authority: before=%+v after=%+v", lease, renewed)
	}

	path += "/phases"
	receipt := phaseReceiptFixture{
		Mutation: tracker.Mutation{IdempotencyKey: "first-phase", LeaseID: lease.ID, FencingToken: lease.FencingToken},
		Phase:    "implement", AttemptID: start.Data.AttemptID, Branch: "detent/example-item",
		HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), PolicyID: lease.PolicyID, InputDigest: strings.Repeat("c", 64),
	}
	for range 2 {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, owner.redemption.Credential, receipt), http.StatusOK)
	}
	read := func(credential string) []phaseReceiptFixture {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, path, credential, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var rows []phaseReceiptFixture
		decodeHubResponse(t, response, &rows)
		return rows
	}
	if rows := read(owner.redemption.Credential); len(rows) != 1 || rows[0].HeadSHA != receipt.HeadSHA || rows[0].TreeSHA != receipt.TreeSHA {
		t.Fatalf("idempotent phase publication = %+v", rows)
	}

	clock.Advance(90 * time.Minute)
	requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPost, path, owner.redemption.Credential, receipt), http.StatusConflict, "stale_execution")
	replacement := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	replacement.enroll(t)
	next := nativeAttemptFixtureLease(t, replacement, issue, "replacement-owner", 90*time.Minute)
	if next.FencingToken <= lease.FencingToken {
		t.Fatalf("replacement fence = %d, previous = %d", next.FencingToken, lease.FencingToken)
	}
	if rows := read(replacement.redemption.Credential); len(rows) != 1 || rows[0].HeadSHA != receipt.HeadSHA || rows[0].PolicyID != lease.PolicyID {
		t.Fatalf("replacement lost pinned carry = %+v", rows)
	}

	other := newNativeFixture(t, f.service, "org_first_second", "Other phase project")
	f.service.localOrganization = string(other.project.OrganizationID)
	otherWorker := prepareRunner(t, other, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	otherWorker.enroll(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		response := performHubAPIRequest(t, f.service, method, path, otherWorker.redemption.Credential, receipt)
		if response.Code != http.StatusForbidden && response.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant phase %s returned %d", method, response.Code)
		}
	}
	f.service.localOrganization = string(f.project.OrganizationID)
	if rows := read(replacement.redemption.Credential); len(rows) != 1 || rows[0].HeadSHA != receipt.HeadSHA {
		t.Fatalf("foreign request changed the pinned phase: %+v", rows)
	}
}

func TestNativeRoundVerdictComparesEvidenceAndCannotOverrideRefusal(t *testing.T) {
	t.Parallel()
	f := newPostgresDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "Keep the same acceptance criterion across repairs")
	worker := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	worker.enroll(t)
	lease := nativeAttemptFixtureLease(t, worker, issue, "round-owner")
	event := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker.redemption.Credential, event), http.StatusOK)

	type roundReceipt struct {
		RoundID    string `json:"round_id"`
		PreviousID string `json:"previous_round_id"`
		Advanced   bool   `json:"advanced"`
		NextPhase  string `json:"next_phase"`
	}
	var previous string
	for _, round := range []struct {
		key    string
		head   string
		action string
		want   int
	}{
		{key: "initial", head: strings.Repeat("a", 40), action: "continue", want: http.StatusOK},
		{key: "different commit same refusal", head: strings.Repeat("b", 40), action: "continue", want: http.StatusOK},
		{key: "unsupported completion", head: strings.Repeat("b", 40), action: "done", want: http.StatusUnprocessableEntity},
	} {
		mutation := tracker.Mutation{IdempotencyKey: round.key, LeaseID: lease.ID, FencingToken: lease.FencingToken}
		input := struct {
			tracker.Mutation
			AttemptID        string                   `json:"attempt_id"`
			RoundID          string                   `json:"round_id"`
			Phase            string                   `json:"phase"`
			HeadSHA          string                   `json:"head_sha"`
			TreeSHA          string                   `json:"tree_sha"`
			PolicyID         string                   `json:"policy_id"`
			FailureSignature string                   `json:"failure_signature"`
			Criteria         []gate.CriterionEvidence `json:"criterion_evidence"`
			Action           string                   `json:"action"`
			NextPhase        string                   `json:"next_phase,omitempty"`
		}{mutation, event.Data.AttemptID, round.key, "project_gate", round.head, strings.Repeat("c", 40), lease.PolicyID, "test:TestAcceptance", []gate.CriterionEvidence{{Criterion: "Complete the requested work.", Kind: gate.EvidenceNotVerified, Reference: "TestAcceptance failed"}}, round.action, "implement"}
		if round.action == "done" {
			input.NextPhase = ""
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", worker.redemption.Credential, input)
		requireNativeStatus(t, response, round.want)
		if round.want == http.StatusOK {
			var receipt roundReceipt
			decodeHubResponse(t, response, &receipt)
			if receipt.RoundID == "" || receipt.PreviousID != previous || receipt.Advanced || receipt.NextPhase != "implement" {
				t.Fatalf("round %q reported progress without evidence or lost prior round: %+v", round.key, receipt)
			}
			previous = receipt.RoundID
		}
	}
	if item := readWorkItem(t, f, issue.WorkItemID, ""); item.State != issue.State {
		t.Fatalf("verdict changed workflow from %s to %s", issue.State, item.State)
	}
}
