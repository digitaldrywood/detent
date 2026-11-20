package hubserver

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

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
	origin, checkout := t.TempDir(), t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		command.Dir = dir
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(origin, "init", "--bare", "--object-format=sha1", "--template=")
	git(checkout, "init", "-b", "detent/example-item", "--object-format=sha1", "--template=")
	git(checkout, "commit", "--allow-empty", "-m", "completed implementation")
	git(checkout, "push", origin, "HEAD:refs/heads/detent/example-item")
	head := git(checkout, "rev-parse", "HEAD")
	tree := git(checkout, "rev-parse", "HEAD^{tree}")
	receipt := phaseReceiptFixture{
		Mutation: tracker.Mutation{IdempotencyKey: "first-phase", LeaseID: lease.ID, FencingToken: lease.FencingToken},
		Phase:    "implement", AttemptID: start.Data.AttemptID, Branch: "detent/example-item",
		HeadSHA: head, TreeSHA: tree, PolicyID: lease.PolicyID, InputDigest: strings.Repeat("c", 64),
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

	git(checkout, "commit", "--allow-empty", "-m", "unfinished work without a phase receipt")
	git(checkout, "push", origin, "HEAD:refs/heads/detent/example-item")
	conflicting := receipt
	conflicting.HeadSHA = git(checkout, "rev-parse", "HEAD")
	requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPost, path, owner.redemption.Credential, conflicting), http.StatusConflict, "idempotency_conflict")

	clock.Advance(90 * time.Minute)
	expiredRenewal := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 5400})
	requireNativeStatus(t, expiredRenewal, http.StatusConflict)
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

	recoverAt := func(credential, digest, policy string) struct {
		LastPushed *phaseReceiptFixture `json:"last_pushed"`
		NextPhase  string               `json:"next_phase"`
	} {
		t.Helper()
		var continuation struct {
			LastPushed *phaseReceiptFixture `json:"last_pushed"`
			NextPhase  string               `json:"next_phase"`
		}
		query := "?phase=implement&phase=project_gate&input_digest=" + digest + "&input_digest=" + digest + "&policy_id=" + policy
		resp := performHubAPIRequest(t, f.service, http.MethodGet, path+query, credential, nil)
		requireNativeStatus(t, resp, http.StatusOK)
		decodeHubResponse(t, resp, &continuation)
		return continuation
	}
	for _, input := range []struct{ digest, policy, phase string }{
		{receipt.InputDigest, receipt.PolicyID, "project_gate"},
		{strings.Repeat("d", 64), receipt.PolicyID, "implement"},
		{receipt.InputDigest, "changed_policy", "implement"},
	} {
		got := recoverAt(replacement.redemption.Credential, input.digest, input.policy)
		if got.LastPushed == nil || got.LastPushed.HeadSHA != head || got.LastPushed.FencingToken != lease.FencingToken || got.NextPhase != input.phase {
			t.Fatalf("replacement continuation = %+v, want head %s and phase %s", got, head, input.phase)
		}
	}
	resumed := t.TempDir()
	git(resumed, "init", "--object-format=sha1", "--template=")
	git(resumed, "fetch", origin, head)
	git(resumed, "checkout", "--detach", "FETCH_HEAD")
	if got := git(resumed, "rev-parse", "HEAD"); got != head || got == conflicting.HeadSHA {
		t.Fatalf("replacement checked out %s, want recorded commit %s", got, head)
	}

	other := newNativeFixture(t, f.service, "org_first_second", "Other phase project")
	f.service.localOrganization = string(other.project.OrganizationID)
	otherWorker := prepareRunner(t, other, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	otherWorker.enroll(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		foreignItem := other.base + "/work-items/" + string(issue.WorkItemID) + "/phases"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, method, foreignItem, otherWorker.redemption.Credential, receipt), http.StatusNotFound)
		response := performHubAPIRequest(t, f.service, method, path, otherWorker.redemption.Credential, receipt)
		if response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden && response.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant phase %s returned %d", method, response.Code)
		}
	}
	approveHubTestPolicy(t, f.service, other.base+"/policy", hubTestPolicy())
	otherIssue := other.create(t, "Record an independent phase")
	otherLease := nativeAttemptFixtureLease(t, otherWorker, otherIssue, "second-tenant")
	otherStart := nativeStartedEvent(otherLease)
	otherPath := other.base + "/work-items/" + string(otherIssue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, otherPath+"/events", otherWorker.redemption.Credential, otherStart), http.StatusOK)
	otherReceipt := receipt
	otherReceipt.LeaseID, otherReceipt.FencingToken, otherReceipt.AttemptID = otherLease.ID, otherLease.FencingToken, otherStart.Data.AttemptID
	otherReceipt.HeadSHA = conflicting.HeadSHA
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, otherPath+"/phases", otherWorker.redemption.Credential, otherReceipt), http.StatusOK)
	otherRead := performHubAPIRequest(t, f.service, http.MethodGet, otherPath+"/phases", otherWorker.redemption.Credential, nil)
	requireNativeStatus(t, otherRead, http.StatusOK)
	var otherRows []phaseReceiptFixture
	decodeHubResponse(t, otherRead, &otherRows)
	if len(otherRows) != 1 || otherRows[0].HeadSHA != otherReceipt.HeadSHA {
		t.Fatalf("second tenant got phase receipts %+v", otherRows)
	}
	f.service.localOrganization = string(f.project.OrganizationID)
	if rows := read(replacement.redemption.Credential); len(rows) != 1 || rows[0].HeadSHA != receipt.HeadSHA {
		t.Fatalf("foreign request changed the pinned phase: %+v", rows)
	}
}
