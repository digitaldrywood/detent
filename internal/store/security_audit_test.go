package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/securityaudit"
)

func TestSQLiteSecurityAuditRunRoundTripAndImmutability(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend, err := Open(ctx, Config{Backend: BackendSQLite, Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	run := securityAuditTestRun()
	recorded, err := backend.RecordSecurityAuditRun(ctx, run)
	if err != nil {
		t.Fatalf("RecordSecurityAuditRun() error = %v", err)
	}
	if recorded.ID <= 0 {
		t.Fatalf("RecordSecurityAuditRun() ID = %d, want positive", recorded.ID)
	}

	key := securityaudit.Key{
		ProjectID:  run.ProjectID,
		Repository: run.Repository,
		PRNumber:   run.PRNumber,
		BaseSHA:    run.BaseSHA,
		HeadSHA:    run.HeadSHA,
	}
	got, err := backend.LatestSecurityAuditRun(ctx, key)
	if err != nil {
		t.Fatalf("LatestSecurityAuditRun() error = %v", err)
	}
	if got.InvocationID != run.InvocationID || got.OutputDigest != run.OutputDigest || len(got.Findings) != 1 || got.Findings[0].ID != "finding-1" {
		t.Fatalf("LatestSecurityAuditRun() = %#v, want persisted run", got)
	}

	disposition, err := backend.RecordSecurityAuditDisposition(ctx, securityaudit.Disposition{
		AuditRunID:      got.ID,
		FindingID:       "finding-1",
		Status:          securityaudit.DispositionFalsePositive,
		Evidence:        "The changed handler is unreachable from untrusted input.",
		ServiceIdentity: "detent:test",
		RecordedAt:      run.RecordedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("RecordSecurityAuditDisposition() error = %v", err)
	}
	dispositions, err := backend.ListSecurityAuditDispositions(ctx, got.ID)
	if err != nil {
		t.Fatalf("ListSecurityAuditDispositions() error = %v", err)
	}
	if len(dispositions) != 1 || dispositions[0].ID != disposition.ID || dispositions[0].Evidence == "" {
		t.Fatalf("ListSecurityAuditDispositions() = %#v, want recorded evidence", dispositions)
	}

	staleKey := key
	staleKey.HeadSHA = "different-head"
	if _, err := backend.LatestSecurityAuditRun(ctx, staleKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestSecurityAuditRun(stale) error = %v, want ErrNotFound", err)
	}

	sqlite := backend.(*sqliteStore)
	immutableMutations := []struct {
		name  string
		query string
	}{
		{name: "update run", query: "UPDATE security_audit_runs SET verdict = 'pass' WHERE id = 1"},
		{name: "delete run", query: "DELETE FROM security_audit_runs WHERE id = 1"},
		{name: "update disposition", query: "UPDATE security_audit_dispositions SET evidence = 'changed' WHERE id = 1"},
		{name: "delete disposition", query: "DELETE FROM security_audit_dispositions WHERE id = 1"},
	}
	for _, mutation := range immutableMutations {
		t.Run(mutation.name, func(t *testing.T) {
			if _, err := sqlite.db.ExecContext(ctx, mutation.query); err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("ExecContext() error = %v, want immutable rejection", err)
			}
		})
	}
}

func TestSQLiteLatestSecurityAuditRunForPullRequest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend, err := Open(ctx, Config{Backend: BackendSQLite, Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	first := securityAuditTestRun()
	if _, err := backend.RecordSecurityAuditRun(ctx, first); err != nil {
		t.Fatalf("RecordSecurityAuditRun(first) error = %v", err)
	}
	second := first
	second.InvocationID = "invocation-2"
	second.HeadSHA = "head-2"
	second.StartedAt = first.StartedAt.Add(time.Minute)
	second.CompletedAt = first.CompletedAt.Add(time.Minute)
	second.RecordedAt = first.RecordedAt.Add(time.Minute)
	if _, err := backend.RecordSecurityAuditRun(ctx, second); err != nil {
		t.Fatalf("RecordSecurityAuditRun(second) error = %v", err)
	}

	got, err := backend.LatestSecurityAuditRunForPullRequest(ctx, first.ProjectID, first.Repository, first.PRNumber)
	if err != nil {
		t.Fatalf("LatestSecurityAuditRunForPullRequest() error = %v", err)
	}
	if got.InvocationID != second.InvocationID || got.HeadSHA != second.HeadSHA {
		t.Fatalf("LatestSecurityAuditRunForPullRequest() = %#v, want latest run", got)
	}
}

func securityAuditTestRun() securityaudit.Run {
	startedAt := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	output := `{"verdict":"fail","summary":"finding","findings":[{"id":"finding-1","severity":"p2","body":"authorization check missing"}]}`
	return securityaudit.Run{
		InvocationID:       "invocation-1",
		ProjectID:          "detent",
		IssueID:            "issue-2005",
		Identifier:         "digitaldrywood/detent#2005",
		IssueURL:           "https://github.test/digitaldrywood/detent/issues/2005",
		Repository:         "digitaldrywood/detent",
		PRNumber:           2006,
		BaseSHA:            "base-1",
		HeadSHA:            "head-1",
		ServiceIdentity:    "detent:test",
		ReviewerVersion:    securityaudit.ReviewerVersion,
		ReviewerDigest:     securityaudit.ReviewerDigest(),
		AuthenticationMode: securityaudit.AuthenticationSubscription,
		WorkerPID:          21,
		WorkerPGID:         21,
		WorkerStartedAt:    startedAt,
		ProviderThreadID:   "thread-1",
		ProviderSessionID:  "session-1",
		ExitStatus:         securityaudit.ExitStatusSuccess,
		OutputDigest:       securityaudit.OutputDigest(output),
		OutputBytes:        len(output),
		Verdict:            securityaudit.VerdictFail,
		Summary:            "finding",
		Findings: []securityaudit.Finding{{
			ID:       "finding-1",
			Severity: "p2",
			Body:     "authorization check missing",
		}},
		Attempt:     1,
		StartedAt:   startedAt,
		CompletedAt: startedAt.Add(time.Second),
		RecordedAt:  startedAt.Add(2 * time.Second),
	}
}
