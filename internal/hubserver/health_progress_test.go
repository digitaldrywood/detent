package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthProgressBackend func(context.Context, string, string) (string, error)

func (f healthProgressBackend) Generate(ctx context.Context, instructions, input string) (string, error) {
	return f(ctx, instructions, input)
}

func TestHealthFindingsRemainDiagnosticAndReachProgressCheck(t *testing.T) {
	t.Parallel()
	for _, signal := range []string{"retry_storm", "scheduler_loop_behind"} {
		t.Run(signal, func(t *testing.T) {
			f := newDispatchGuardFixture(t)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{
				Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "failed",
				TerminalFailure: &tracker.NativeTerminalFailure{Error: "publication refused", ErrorClass: "publication", Stage: "publication", ObservedAt: f.now},
			}}
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, event), http.StatusOK)
			subject := healthSubject{Kind: "work_item", ID: string(f.issue.WorkItemID)}
			if signal == "scheduler_loop_behind" {
				subject = healthSubject{Kind: "project", ID: string(f.project.ID)}
			}
			finding := healthFinding{Fingerprint: signal, Signal: signal, Class: "instance", Subject: subject, Projects: []string{string(f.project.ID)}, Severity: "attention", Summary: "Recorded failure", Evidence: healthEvidence{Counts: map[string]int{"failures": 5}}}
			var initialCount int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues").Scan(&initialCount); err != nil {
				t.Fatal(err)
			}
			if err := f.service.commitHealthEvaluation(t.Context(), scope.organization, f.now, []healthFinding{finding}, nil); err != nil {
				t.Fatal(err)
			}
			var evaluatedCount int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues").Scan(&evaluatedCount); err != nil || evaluatedCount != initialCount {
				t.Fatalf("detector filed %d issues: %v", evaluatedCount-initialCount, err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var findingID string
			if err := tx.QueryRowContext(t.Context(), "SELECT id FROM health_findings WHERE organization_id=? AND fingerprint=?", scope.organization, finding.Fingerprint).Scan(&findingID); err != nil {
				t.Fatal(err)
			}
			legacyScope := scope
			legacyScope.sourceActor = nativeIntegrationActor("health_detector")
			body := issueorigin.Stamp("Legacy diagnostic report.\n\n<!-- detent:no-dispatch -->", issueorigin.Origin{Kind: "audit", Instance: "health_detector", Source: findingID, Fingerprint: finding.Fingerprint})
			legacy, err := createNativeFixtureIssueTx(t.Context(), tx, legacyScope, tracker.CreateIssue{Title: "intake(instance): legacy", Body: body, State: "Todo"}, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), "INSERT INTO health_finding_issues(finding_id,project_id,work_item_id,reported_evidence_digest,intake_fingerprint) VALUES(?,?,?,'',?)", findingID, scope.project, legacy.WorkItemID, finding.Fingerprint); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			before, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, string(legacy.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			var countBefore int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues").Scan(&countBefore); err != nil {
				t.Fatal(err)
			}
			for _, offset := range []time.Duration{0, time.Minute} {
				if err := f.service.commitHealthEvaluation(t.Context(), f.project.OrganizationID, f.now.Add(offset), []healthFinding{finding}, nil); err != nil {
					t.Fatal(err)
				}
			}
			page, err := f.service.readHealthFindings(t.Context(), scope, "open", "", "", 100)
			if err != nil || len(page.Items) != 1 || page.Items[0].Signal != signal {
				t.Fatalf("health read=%+v err=%v", page, err)
			}
			var countAfter int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues").Scan(&countAfter); err != nil {
				t.Fatal(err)
			}
			if countAfter != countBefore {
				t.Fatalf("detector filed %d issues", countAfter-countBefore)
			}
			after, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, string(legacy.WorkItemID))
			if err != nil || after.Revision != before.Revision || after.Body != before.Body || after.State != before.State {
				t.Fatalf("legacy issue modified: %+v err=%v", after, err)
			}
			request, _, err := f.service.nativeProgressRequest(t.Context(), scope, string(f.issue.WorkItemID), f.policy, string(f.lease.MachineID))
			if err != nil {
				t.Fatal(err)
			}
			called := false
			_, err = (progresscheck.AI{Backend: healthProgressBackend(func(_ context.Context, _ string, input string) (string, error) {
				called = true
				var got progresscheck.Request
				if err := json.Unmarshal([]byte(input), &got); err != nil {
					t.Fatal(err)
				}
				for _, receipt := range got.Receipts {
					if receipt.ID == page.Items[0].ID && receipt.Step == "health" && receipt.Outcome == signal && strings.Contains(receipt.Detail, `"failures":5`) {
						raw, err := json.Marshal(progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Diagnosis: receipt.ID + ": recorded health evidence shows repeated publication failures or scheduler delay", Instruction: "Inspect the recorded finding and repair the cause before repeating the failed step", Reason: "Health evidence requires investigation", Evidence: []string{receipt.ID}})
						return string(raw), err
					}
				}
				t.Fatalf("missing health evidence: %+v", got.Receipts)
				return "", nil
			})}).Decide(t.Context(), request)
			if err != nil || !called {
				t.Fatalf("progress backend called=%v err=%v", called, err)
			}
			if err := f.service.commitHealthEvaluation(t.Context(), f.project.OrganizationID, f.now.Add(2*time.Minute), nil, nil); err != nil {
				t.Fatal(err)
			}
			after, _, err = readNativeIssue(t.Context(), f.service.database.reader, scope, string(legacy.WorkItemID))
			if err != nil || after.Revision != before.Revision || after.Body != before.Body || after.State != before.State {
				t.Fatalf("resolution modified legacy issue: %+v err=%v", after, err)
			}
			var comments int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id=?", legacy.WorkItemID).Scan(&comments); err != nil || comments != 0 {
				t.Fatalf("legacy issue comments=%d err=%v", comments, err)
			}
			receipts, err := f.service.progressHealthReceipts(t.Context(), scope, string(f.issue.WorkItemID))
			if err != nil || len(receipts) != 0 {
				t.Fatalf("resolved findings supplied: %+v %v", receipts, err)
			}
		})
	}
}

func TestHealthProgressEvidenceScope(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "health-progress-scope")
	f.service.stopHealthDetector()
	item := f.create(t, "current item")
	other := f.create(t, "other item")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	findings := []healthFinding{
		{Fingerprint: "direct", Signal: "retry_storm", Subject: healthSubject{Kind: "work_item", ID: string(item.WorkItemID)}, Projects: []string{string(f.project.ID)}},
		{Fingerprint: "other", Signal: "retry_storm", Subject: healthSubject{Kind: "work_item", ID: string(other.WorkItemID)}, Projects: []string{string(f.project.ID)}},
		{Fingerprint: "shared", Signal: "every_candidate_refused", Subject: healthSubject{Kind: "instance", ID: "example-instance"}, Projects: []string{string(f.project.ID)}, Evidence: healthEvidence{Queues: map[string]healthQueueEvidence{
			string(f.project.ID): {QueueDepth: 2}, "other-project": {QueueDepth: 99},
		}}},
		{Fingerprint: "unrelated-project", Signal: "scheduler_loop_behind", Subject: healthSubject{Kind: "project", ID: "other-project"}},
	}
	for i := range findings {
		findings[i].Class = "instance"
		findings[i].Severity = "attention"
	}
	if err := f.service.commitHealthEvaluation(t.Context(), scope.organization, f.service.config.now(), findings, nil); err != nil {
		t.Fatal(err)
	}
	receipts, err := f.service.progressHealthReceipts(t.Context(), scope, string(item.WorkItemID))
	if err != nil || len(receipts) != 2 {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	if receipts[0].Outcome != "retry_storm" || !strings.Contains(receipts[0].Detail, string(item.WorkItemID)) {
		t.Fatalf("direct evidence lost: %+v", receipts)
	}
	if receipts[1].Outcome != "every_candidate_refused" || !strings.Contains(receipts[1].Detail, `"queue_depth":2`) || strings.Contains(receipts[1].Detail, "other-project") {
		t.Fatalf("shared evidence scope: %+v", receipts[1])
	}
}
