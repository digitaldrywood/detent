package hubserver

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeProgressPublicationIsAtomicAndScoped(t *testing.T) {
	t.Parallel()
	var offset atomic.Int64
	clock := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return clock.Add(time.Duration(offset.Load())) }})
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Backlog','Backlog',0,0,?,?)", f.project.ID, testTimestamp, testTimestamp); err != nil {
		t.Fatal(err)
	}
	states, err := json.Marshal(append(f.project.States, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", string(states), f.project.ID); err != nil {
		t.Fatal(err)
	}

	credential, _, err := f.service.authenticateAPIToken(t.Context(), testHubAdminToken, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	var previousIssue string
	for _, outcome := range []progresscheck.Outcome{progresscheck.Progressing, progresscheck.Reroute, progresscheck.SystemDefect} {
		t.Run(string(outcome), func(t *testing.T) {
			issue := f.createIndependent(t, "Example "+string(outcome))
			request := progresscheck.Request{ProjectID: string(f.project.ID), IssueID: string(issue.WorkItemID), PolicyID: "policy", EvidenceRevision: "revision", AvailableSteps: []string{"implement"}, Attempts: []progresscheck.Attempt{{ID: "failed-attempt", Outcome: "failed"}}}
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			decision := progresscheck.Decision{Outcome: outcome, Reason: "Source or evidence requires another step", Evidence: []string{"failed-attempt"}}
			if outcome == progresscheck.Reroute {
				decision.Step = "implement"
			}
			if outcome == progresscheck.SystemDefect {
				decision.DefectKey = "example-protocol"
				decision.DefectTitle = "Example protocol refusal"
				decision.DefectBody = issueContractTestSections
			}
			owner := nativeProgressOwner{service: f.service, scope: scope, request: request}
			record := progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: decision}
			if _, err := f.service.database.db.ExecContext(t.Context(), "CREATE TRIGGER reject_progress_evidence BEFORE INSERT ON native_progress_evidence BEGIN SELECT RAISE(ABORT, 'injected failure'); END"); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Publish(t.Context(), record); err == nil {
				t.Fatal("injected storage failure accepted")
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER reject_progress_evidence"); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{
				"SELECT count(*) FROM native_comments WHERE work_item_id = ?",
				"SELECT count(*) FROM native_progress_decisions WHERE work_item_id = ?",
			} {
				var count int
				if err := f.service.database.reader.QueryRowContext(t.Context(), query, issue.WorkItemID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("publication failure retained %d rows", count)
				}
			}
			if outcome == progresscheck.SystemDefect {
				var count int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id = ? AND title = ?", f.project.ID, decision.DefectTitle).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("publication failure retained %d defects", count)
				}
			}

			published, err := owner.Publish(t.Context(), record)
			if err != nil {
				t.Fatal(err)
			}
			if err := published.Validate(request); err != nil {
				t.Fatal(err)
			}
			replay, err := owner.Publish(t.Context(), record)
			if err != nil || !reflect.DeepEqual(published, replay) {
				t.Fatalf("replay = %+v, %v", replay, err)
			}
			read, found, err := owner.Load(t.Context(), request.ProjectID, request.IssueID, id)
			if err != nil || !found || !reflect.DeepEqual(read, published) {
				t.Fatalf("read = %+v, found %v, %v", read, found, err)
			}
			var comments int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id = ?", issue.WorkItemID).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if comments != 1 {
				t.Fatalf("replay published %d comments", comments)
			}
			if previousIssue != "" {
				_, found, err := readNativeProgress(t.Context(), f.service.database.reader, scope, previousIssue, id)
				if err != nil || found {
					t.Fatalf("another issue's decision leaked: found %v, %v", found, err)
				}
			}
			previousIssue = request.IssueID
			if outcome != progresscheck.SystemDefect {
				return
			}
			if published.DefectIssueID == "" {
				t.Fatal("missing defect receipt")
			}
			comment, err := readNativeComment(t.Context(), f.service.database.reader, scope, request.IssueID, published.CommentID)
			if err != nil {
				t.Fatal(err)
			}
			defectIssue, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, published.DefectIssueID)
			if err != nil || !strings.Contains(comment.Body, "]("+defectIssue.WebURL+")") {
				t.Fatalf("missing defect link: comment=%q error=%v", comment.Body, err)
			}
			request.EvidenceRevision = "new-human-input"
			newID, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			record.RequestID, record.EvidenceRevision = newID, request.EvidenceRevision
			owner.request = request
			second, err := owner.Publish(t.Context(), record)
			if err != nil || second.DefectIssueID != published.DefectIssueID {
				t.Fatalf("defect deduplication = %+v, %v", second, err)
			}
			offset.Store(int64(100 * time.Millisecond))
			request.EvidenceRevision = "different-defect"
			record.RequestID, err = request.ID()
			if err != nil {
				t.Fatal(err)
			}
			record.EvidenceRevision = request.EvidenceRevision
			record.Decision.DefectKey = "different-protocol"
			owner.request = request
			latest, err := owner.Publish(t.Context(), record)
			if err != nil {
				t.Fatal(err)
			}
			defect, state, err := readNativeProgressDefect(t.Context(), f.service.database.reader, scope, request.IssueID)
			if err != nil || defect != latest.DefectIssueID || state != "Backlog" {
				t.Fatalf("latest defect=%s state=%s error=%v", defect, state, err)
			}
		})
	}
}
