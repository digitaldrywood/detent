package hubserver

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
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

func TestNativeExecutionPublicationsShareWorkerLockOrder(t *testing.T) {
	t.Parallel()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Postgres.Pool.MaxConnections = 8
	cfg.Postgres.Pool.ServingConnections = 4
	seedHostedStartup(t, cfg)
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	f := seedRunnerPGFixture(t, admin, service, "org_startup")
	lease, attempt := f.leases[0], f.data[0].AttemptID
	base := "/api/v2/organizations/" + string(f.organization) + "/projects/prj_runner"
	item := base + "/work-items/" + string(lease.WorkItemID)
	fence := tracker.Mutation{IdempotencyKey: "publication", LeaseID: lease.ID, FencingToken: lease.FencingToken}
	receipt := phaseReceiptFixture{
		Mutation: fence, Phase: "implement", AttemptID: attempt, PolicyID: lease.PolicyID,
		Branch: "detent/example-item", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40),
		InputDigest: strings.Repeat("c", 64),
	}
	verdict := progresscheck.Publication{
		Mutation: fence, AttemptID: attempt, PolicyID: lease.PolicyID,
		RoundInput: progresscheck.RoundInput{RoundID: "setup", Phase: "workspace_preparation", Action: "continue", NextPhase: "implement"},
	}
	for _, publication := range []struct {
		path  string
		input any
	}{
		{path: "phases", input: receipt},
		{path: "progress", input: verdict},
	} {
		t.Run(publication.path, func(t *testing.T) {
			ctx := testenv.WaitContext(t, t.Context())
			barrier, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			var workers sync.WaitGroup
			t.Cleanup(func() {
				_ = barrier.Rollback()
				workers.Wait()
			})
			var barrierPID int
			if err := barrier.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&barrierPID); err != nil {
				t.Fatal(err)
			}
			if _, err := barrier.ExecContext(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR NO KEY UPDATE`, f.organization, "prj_runner"); err != nil {
				t.Fatal(err)
			}
			post := func(path string, input any) <-chan *httptest.ResponseRecorder {
				request := newHubAPIRequest(t, http.MethodPost, path, f.tokens[0], input).WithContext(ctx)
				result := make(chan *httptest.ResponseRecorder, 1)
				workers.Go(func() {
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					result <- response
				})
				return result
			}
			waitForLock := func(blocker, queued int, queryPart string, result <-chan *httptest.ResponseRecorder) int {
				t.Helper()
				var pid int
				testenv.WaitUntil(t, ctx, func() error {
					select {
					case response := <-result:
						t.Fatalf("request finished before waiting for %s: %d %s", queryPart, response.Code, response.Body)
					default:
					}
					if queued != 0 {
						var projectWait bool
						err := f.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>$1 AND wait_event_type='Lock' AND ($1=ANY(pg_blocking_pids(pid)) OR $2=ANY(pg_blocking_pids(pid))) AND query LIKE '%name: LockWorkItemProject%')`, blocker, barrierPID).Scan(&projectWait)
						if err != nil {
							return err
						}
						if projectWait {
							t.Fatal("worker comment waited on the project before locking the runner")
						}
					}
					err := f.admin.QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND pid<>$2 AND ($1=ANY(pg_blocking_pids(pid)) OR $2=ANY(pg_blocking_pids(pid))) AND query LIKE $3`, blocker, queued, "%"+queryPart+"%").Scan(&pid)
					if errors.Is(err, sql.ErrNoRows) {
						return errors.New("waiting for " + queryPart + " row lock")
					}
					return err
				})
				return pid
			}
			published := post(item+"/"+publication.path, publication.input)
			publisherPID := waitForLock(barrierPID, 0, "name: LockWorkItemProject", published)
			renewed := post(base+"/leases/"+string(lease.ID)+"/renew", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 5400})
			renewalPID := waitForLock(publisherPID, 0, "runner_identities", renewed)
			comment := tracker.CreateComment{
				Mutation: tracker.Mutation{IdempotencyKey: publication.path + "-comment", LeaseID: lease.ID, FencingToken: lease.FencingToken},
				Body:     "Example worker update",
			}
			commented := post(item+"/comments", comment)
			waitForLock(publisherPID, renewalPID, "runner_identities", commented)
			if err := barrier.Commit(); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, <-published, http.StatusOK)
			requireNativeStatus(t, <-renewed, http.StatusOK)
			requireNativeStatus(t, <-commented, http.StatusOK)
			var deadlocks int
			if err := f.admin.QueryRowContext(ctx, `SELECT deadlocks FROM pg_stat_database WHERE datname=current_database()`).Scan(&deadlocks); err != nil {
				t.Fatal(err)
			}
			if deadlocks != 0 {
				t.Fatalf("PostgreSQL reported %d deadlocks", deadlocks)
			}
		})
	}
	receipt.FencingToken++
	requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPost, item+"/phases", f.tokens[0], receipt), http.StatusConflict, "stale_execution")
}
