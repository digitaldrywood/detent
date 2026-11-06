package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type progressLeaseOrderWriter struct {
	tenantstore.Writer
	issue, lease, issueWait chan struct{}
	hasIssue, hasLease      bool
}

func (w *progressLeaseOrderWriter) AccessCredential(context.Context, string) (accessstore.Credential, error) {
	return accessstore.Credential{}, nil
}

func (w *progressLeaseOrderWriter) RunnerIdentityByCredential(context.Context, string) (runnerstore.Identity, error) {
	return runnerstore.Identity{}, nil
}

func (w *progressLeaseOrderWriter) RunnerForLease(context.Context, string) (runnerstore.Identity, error) {
	return runnerstore.Identity{}, nil
}

func (w *progressLeaseOrderWriter) LockWorkItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	close(w.issueWait)
	<-w.issue
	w.hasIssue = true
	return corestore.WorkItem{}, nil
}

func (w *progressLeaseOrderWriter) LockLease(context.Context, string) (leasestore.Lease, error) {
	<-w.lease
	w.hasLease = true
	return leasestore.Lease{}, nil
}

func TestProgressLeaseValidationDoesNotDeadlockClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writer := &progressLeaseOrderWriter{issue: make(chan struct{}, 1), lease: make(chan struct{}, 1), issueWait: make(chan struct{})}
		writer.lease <- struct{}{}
		owner := nativeProgressOwner{service: &Service{config: Config{now: time.Now}}, scope: nativeScope{credential: apiCredential{Scope: apiScopeWorker}}}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if writer.hasLease {
					writer.lease <- struct{}{}
				}
				if writer.hasIssue {
					writer.issue <- struct{}{}
				}
			}()
			_ = owner.validateProgressLeaseWriter(t.Context(), writer)
		}()
		<-writer.issueWait
		synctest.Wait()
		claimDone := make(chan struct{})
		go func() {
			<-writer.lease
			writer.lease <- struct{}{}
			close(claimDone)
		}()
		synctest.Wait()
		select {
		case <-claimDone:
		default:
			t.Error("claim could not acquire its lease while progress waited for the issue")
		}
		writer.issue <- struct{}{}
		<-done
		<-claimDone
	})
}

type sharedOutageProgressBackend struct {
	decision progresscheck.Decision
}

func (b sharedOutageProgressBackend) Decide(context.Context, progresscheck.Request) (progresscheck.Decision, error) {
	return b.decision, nil
}

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
	var historyIndexes int
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_schema WHERE type='index' AND name IN ('native_comments_progress_input','native_attempts_progress_success','native_progress_human_input')`).Scan(&historyIndexes); err != nil {
		t.Fatal(err)
	}
	if historyIndexes != 0 {
		t.Fatalf("progress migration builds %d history indexes at startup", historyIndexes)
	}
	var previousIssue string
	for _, outcome := range []progresscheck.Outcome{progresscheck.Progressing, progresscheck.Reroute, progresscheck.SystemDefect} {
		t.Run(string(outcome), func(t *testing.T) {
			issue := f.createIndependent(t, "Example "+string(outcome))
			request := progresscheck.Request{ProjectID: string(f.project.ID), IssueID: string(issue.WorkItemID), PolicyID: "policy", EvidenceRevision: "revision", AvailableSteps: []string{"implement"}, Attempts: []progresscheck.Attempt{{ID: "failed-attempt", Outcome: "failed"}}}
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			decision := progresscheck.Decision{Outcome: outcome, Diagnosis: "failed-attempt" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the source finalization check and retain its command receipt", Convergence: "The previously failing check has a successful receipt on the repaired assertion", Reason: "Source or evidence requires another step", Evidence: []string{"failed-attempt"}}
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
			revision, err := progressRevision(t.Context(), f.service.database.reader, scope, request.IssueID)
			if err != nil {
				t.Fatal(err)
			}
			owner.guardRevision = &revision
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET progress_evidence_revision=progress_evidence_revision+1 WHERE native_id=?", issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if _, err := owner.Publish(t.Context(), record); err == nil {
				t.Fatal("new source evidence was accepted under the old publication revision")
			}
			for _, eventType := range []string{"run.started", "run.observed", "workflow.transitioned", "run.finished", "run.checkpointed"} {
				revision, err = progressRevision(t.Context(), f.service.database.reader, scope, request.IssueID)
				if err != nil {
					t.Fatal(err)
				}
				contentCtx := f.service.runDetailContext(t.Context())
				if err := f.service.hubTransact(contentCtx, func(tx *sql.Tx, _ time.Time) error {
					return appendNativeHistory(contentCtx, tx, scope, request.IssueID, eventType, tracker.CollaborationData{}, clock)
				}); err != nil {
					t.Fatal(err)
				}
				current, err := progressRevision(t.Context(), f.service.database.reader, scope, request.IssueID)
				if err != nil {
					t.Fatal(err)
				}
				changed := eventType == "run.finished" || eventType == "run.checkpointed"
				if (current != revision) != changed {
					t.Fatalf("%s changed evidence revision from %d to %d", eventType, revision, current)
				}
				if changed {
					if _, err := owner.Publish(t.Context(), record); err == nil {
						t.Fatalf("%s bypassed the transaction guard", eventType)
					}
				}
			}
			owner.guardRevision = nil

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
			dependent, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, request.IssueID)
			if err != nil || len(dependent.Dependencies) != 1 || string(dependent.Dependencies[0]) != published.DefectIssueID || dependent.DependencyReady {
				t.Fatalf("progress defect has no dispatch dependency: %+v, %v", dependent, err)
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
			t.Run("shared outage across distinct issues creates one defect", func(t *testing.T) {
				backend := sharedOutageProgressBackend{decision: decision}
				for index := range 6 {
					other := f.createIndependent(t, fmt.Sprintf("Outage issue %d", index))
					outage := request
					outage.IssueID = string(other.WorkItemID)
					outage.Instance = "example-instance"
					outage.Attempts = []progresscheck.Attempt{
						{ID: "failed-attempt", Step: "implement", Outcome: "failed", Error: fmt.Sprintf("backend protocol unavailable: request-%d", index), Attribution: "instance", Instance: outage.Instance},
						{ID: "failed-retry", Step: "implement", Outcome: "failed", Error: fmt.Sprintf("backend protocol unavailable: retry-%d", index), Attribution: "instance", Instance: outage.Instance},
					}
					otherOwner := nativeProgressOwner{service: f.service, scope: scope, request: outage}
					result, checked, err := progresscheck.Check(t.Context(), outage, backend, otherOwner)
					if err != nil || !checked || result.DefectIssueID != published.DefectIssueID || result.CommentID == published.CommentID {
						t.Fatalf("issue %s did not reuse the shared defect: result=%+v checked=%t error=%v", other.WorkItemID, result, checked, err)
					}
					dependent, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, outage.IssueID)
					if err != nil || len(dependent.Dependencies) != 1 || string(dependent.Dependencies[0]) != published.DefectIssueID || dependent.DependencyReady {
						t.Fatalf("issue %s lost its shared repair dependency: %+v, %v", other.WorkItemID, dependent, err)
					}
				}
				var defects int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=? AND title=?", f.project.ID, decision.DefectTitle).Scan(&defects); err != nil {
					t.Fatal(err)
				}
				if defects != 1 {
					t.Fatalf("shared outage created %d defect records, want 1", defects)
				}
			})
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
			for _, eventType := range []string{"comment.created", "comment.edited", "change.version_published", "run.checkpointed", "issue.edited"} {
				request.EvidenceRevision = eventType
				record.RequestID, err = request.ID()
				if err != nil {
					t.Fatal(err)
				}
				record.EvidenceRevision = request.EvidenceRevision
				owner.request = request
				if _, err := owner.Publish(t.Context(), record); err != nil {
					t.Fatal(err)
				}
				linked, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, request.IssueID)
				if err != nil || linked.DependencyReady || len(linked.Dependencies) != 1 {
					t.Fatalf("missing dependency before %s: %v", eventType, err)
				}
				ctx := f.service.runDetailContext(t.Context())
				if err := f.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
					return appendNativeHistory(ctx, tx, scope, request.IssueID, eventType, tracker.CollaborationData{}, clock)
				}); err != nil {
					t.Fatal(err)
				}
				reopened, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, request.IssueID)
				if err != nil || !reopened.DependencyReady || len(reopened.Dependencies) != 0 {
					t.Fatalf("%s retained progress veto: %v", eventType, err)
				}
			}
			request.EvidenceRevision = "defect-landed"
			record.RequestID, err = request.ID()
			if err != nil {
				t.Fatal(err)
			}
			record.EvidenceRevision = request.EvidenceRevision
			owner.request = request
			if _, err := owner.Publish(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Done') WHERE native_id=?`, scope.project, latest.DefectIssueID); err != nil {
				t.Fatal(err)
			}
			reopened, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, request.IssueID)
			if err != nil || !reopened.DependencyReady {
				t.Fatalf("landed defect still vetoes dispatch: %v", err)
			}
			ordinary := f.createIndependent(t, "Ordinary dependency")
			ctx := f.service.contentContext(t.Context())
			if err := f.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
				if _, err := changeNativeDependencyTx(ctx, tx, scope, request.IssueID, tracker.DependencyMutation{ExpectedRevision: reopened.Revision, Operation: "add", RelatedWorkItemID: ordinary.WorkItemID}, clock); err != nil {
					return err
				}
				return appendNativeHistory(ctx, tx, scope, request.IssueID, "issue.edited", tracker.CollaborationData{}, clock)
			}); err != nil {
				t.Fatal(err)
			}
			retained, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, request.IssueID)
			if err != nil || retained.DependencyReady || len(retained.Dependencies) != 1 || retained.Dependencies[0] != ordinary.WorkItemID {
				t.Fatalf("new evidence changed ordinary dependency: %v", err)
			}

		})
	}
}

func TestNativeProgressRetrievedEvidenceIsScoped(t *testing.T) {
	for _, test := range []struct {
		name      string
		source    string
		retrieved bool
		valid     bool
	}{
		{"retrieved same item", "same", true, true},
		{"source receipt", "source", true, true},
		{"forged source receipt ID", "source forged", true, false},
		{"source receipt differs from pinned source", "source mismatch", true, false},
		{"source receipt missing proof", "source unproven", true, false},
		{"same item never retrieved", "same", false, false},
		{"cross item", "item", true, false},
		{"cross project", "project", true, false},
		{"nonexistent", "missing", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			issue := f.createIndependent(t, "Example target")
			other := f.createIndependent(t, "Example other")
			source, item := f, issue
			if test.source == "item" {
				item = other
			}
			if test.source == "project" {
				source = newNativeFixture(t, f.service, f.project.OrganizationID, "Example other project")
				item = source.createIndependent(t, "Example project item")
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, source.base+"/work-items/"+string(item.WorkItemID)+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "retrieved-comment"}, Body: "Assertion still returns the old value"})
			requireNativeStatus(t, response, http.StatusOK)
			var comment tracker.NativeComment
			decodeHubResponse(t, response, &comment)
			citation := comment.ID
			if test.source == "missing" {
				citation = "comment-missing"
			}
			request := progresscheck.Request{ProjectID: string(f.project.ID), IssueID: string(issue.WorkItemID), PolicyID: "policy", EvidenceRevision: "revision", Attempts: []progresscheck.Attempt{{ID: "failed-attempt", Outcome: "failed"}}, AvailableSteps: []string{"implement"}}
			var sourceReference progresscheck.EvidenceReference
			if strings.HasPrefix(test.source, "source") {
				request.SourceSHA = strings.Repeat("a", 40)
				started := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
				timing := gate.PipelineTiming{Command: "git show HEAD:example.go", HeadSHA: request.SourceSHA, TreeSHA: strings.Repeat("b", 40), StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: new(0), Execution: "executed"}
				timing.ReceiptID = timing.ID()
				citation = timing.ReceiptID
				sourceReference = progresscheck.EvidenceReference{ID: citation, SourceSHA: request.SourceSHA, SourceReceipt: &timing}
				if test.source == "source forged" {
					citation, sourceReference.ID = "timing_forged", "timing_forged"
				}
				if test.source == "source mismatch" {
					request.SourceSHA = strings.Repeat("c", 40)
				}
				if test.source == "source unproven" {
					sourceReference.SourceReceipt = nil
				}
			}
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			decision := progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Reason: "Repair assertion", Diagnosis: citation + ": assertion returns the old value", Instruction: "Repair the implementation and run TestExample", Evidence: []string{citation}}
			if test.retrieved {
				decision.RetrievedEvidence = []progresscheck.EvidenceReference{{ID: citation}}
			}
			if strings.HasPrefix(test.source, "source") {
				decision.RetrievedEvidence = []progresscheck.EvidenceReference{sourceReference}
			}
			owner := nativeProgressOwner{service: f.service, scope: nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}, request: request}
			record, err := owner.Publish(t.Context(), progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: decision})
			if (err == nil) != test.valid {
				t.Fatalf("publication error=%v valid=%v", err, test.valid)
			}
			retained, found, err := owner.Load(t.Context(), request.ProjectID, request.IssueID, id)
			if err != nil || found != test.valid {
				t.Fatalf("retained invalid publication: found=%v error=%v", found, err)
			}
			if test.valid {
				if err := retained.Validate(request); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(record, retained) {
					t.Fatalf("replay lost retrieved evidence: %+v", retained)
				}
			}
		})
	}
}

type progressBlockerProbeWriter struct {
	tenantstore.Writer
	expected map[int64]bool
	seen     map[int64]bool
	pages    int
}

func (w *progressBlockerProbeWriter) WorkItemBlockers(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cursor corestore.WorkItemBlockerCursor, limit int) ([]corestore.WorkItemBlocker, error) {
	rows, err := w.Writer.WorkItemBlockers(ctx, project, item, cursor, limit)
	w.pages++
	for _, row := range rows {
		if !w.expected[row.IssueID] {
			return nil, errors.New("blocker query crossed tenant scope")
		}
		w.seen[row.IssueID] = true
	}
	return rows, err
}

func TestPostgresProgressLocksGraphBeforeScopedEndpoints(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	repository := store.postgres
	seedStartupOrganization(t, repository, "org_progress_control")
	for _, organization := range []tracker.OrganizationID{"org_progress_first", "org_progress_second"} {
		seedStartupOrganization(t, repository, organization)
		expected := map[int64]bool{}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example " + string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
			}
			create := func(project tracker.ProjectID, id tracker.NativeWorkItemID, number int64, source *string) (corestore.WorkItem, error) {
				return w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: id, Number: number, Revision: 1, Title: "Example", WorkflowStateName: new("Todo"), NativeSourceKey: source})
			}
			for i, id := range []tracker.NativeWorkItemID{"wi_primary", "wi_next", "wi_shadow"} {
				if _, err := create("prj_first", id, int64(i+1), nil); err != nil {
					return err
				}
			}
			for i := range 201 {
				project, id, number, source := tracker.ProjectID("prj_first"), tracker.NativeWorkItemID(fmt.Sprintf("wi_blocker_%d", i)), int64(i+4), (*string)(nil)
				if i == 0 {
					source = new("progress-check:local")
				}
				if i == 1 {
					project, id, number, source = "prj_second", "wi_shared", 1, new("progress-check:foreign-project")
				}
				row, err := create(project, id, number, source)
				if err != nil {
					return err
				}
				expected[row.ID] = true
				if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: organization, ProjectID: "prj_first", WorkItemID: "wi_primary", RelatedProjectID: project, RelatedWorkItemID: id, Type: "dependency"}, "add", time.Now().UTC()); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		held, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		var tasks sync.WaitGroup
		graphResult, progressResult := make(chan error, 1), make(chan error, 1)
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			graphResult <- repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.LockRelationshipGraph(ctx); err != nil {
					return err
				}
				close(held)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		t.Cleanup(func() { once.Do(func() { close(release) }); cancel(); tasks.Wait() })
		testenv.WaitUntil(t, t.Context(), func() error {
			select {
			case <-held:
				return nil
			default:
				return errors.New("graph holder has not acquired its lock")
			}
		})
		fk, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var organizationID string
		err = fk.QueryRowContext(t.Context(), `SELECT organization_id FROM organizations WHERE organization_id=$1 FOR KEY SHARE NOWAIT`, organization).Scan(&organizationID)
		if err == nil {
			_, err = fk.ExecContext(t.Context(), `INSERT INTO machines(organization_id,id,hostname,display_name,capacity,version,sprite_name,last_heartbeat_at,registered_at,updated_at) VALUES($1,'machine_fk_probe','example','',1,'test','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, organization)
		}
		rollbackErr := fk.Rollback()
		if err != nil {
			t.Fatalf("graph lock blocked an unrelated organization foreign-key reference: %v", err)
		}
		if rollbackErr != nil {
			t.Fatal(rollbackErr)
		}
		orgUpdate, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = orgUpdate.QueryRowContext(t.Context(), `SELECT organization_id FROM organizations WHERE organization_id=$1 FOR NO KEY UPDATE NOWAIT`, organization).Scan(&organizationID)
		rollbackErr = orgUpdate.Rollback()
		var updateLock *pq.Error
		if !errors.As(err, &updateLock) || updateLock.Code != "55P03" {
			t.Fatalf("graph lock did not serialize organization updates: %v", err)
		}
		if rollbackErr != nil {
			t.Fatal(rollbackErr)
		}
		otherGraph, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = otherGraph.QueryRowContext(t.Context(), `SELECT organization_id FROM organizations WHERE organization_id='org_progress_control' FOR NO KEY UPDATE NOWAIT`).Scan(&organizationID)
		rollbackErr = otherGraph.Rollback()
		if err != nil {
			t.Fatalf("graph lock crossed tenant scope: %v", err)
		}
		if rollbackErr != nil {
			t.Fatal(rollbackErr)
		}
		owner := nativeProgressOwner{scope: nativeScope{organization: organization, project: "prj_first"}}
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			progressResult <- repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
				probe := &progressBlockerProbeWriter{Writer: w, expected: expected, seen: map[int64]bool{}}
				started := time.Now()
				removals, err := owner.lockProgressRelationships(ctx, probe, progresscheck.Record{ProjectID: "prj_first", IssueID: "wi_primary", DefectIssueID: "wi_next"})
				if err != nil {
					return err
				}
				if probe.pages != 2 || len(probe.seen) != 201 || len(removals) != 2 {
					return fmt.Errorf("progress endpoints pages=%d blockers=%d removals=%d", probe.pages, len(probe.seen), len(removals))
				}
				for _, endpoint := range []corestore.WorkItemReference{{ProjectID: "prj_first", WorkItemID: "wi_primary"}, {ProjectID: "prj_first", WorkItemID: "wi_next"}, {ProjectID: "prj_first", WorkItemID: "wi_blocker_0"}, {ProjectID: "prj_second", WorkItemID: "wi_shared"}} {
					tx, err := admin.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					var id int64
					err = tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 FOR UPDATE NOWAIT`, organization, endpoint.ProjectID, endpoint.WorkItemID).Scan(&id)
					rollbackErr := tx.Rollback()
					var locked *pq.Error
					if !errors.As(err, &locked) || locked.Code != "55P03" {
						return fmt.Errorf("progress endpoint was not locked: %v", err)
					}
					if rollbackErr != nil {
						return rollbackErr
					}
				}
				tx, err := admin.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				var shadow int64
				err = tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE organization_id=$1 AND project_id='prj_first' AND native_id='wi_shadow' FOR UPDATE NOWAIT`, organization).Scan(&shadow)
				rollbackErr := tx.Rollback()
				if err != nil {
					return fmt.Errorf("progress locked an unrelated endpoint: %w", err)
				}
				if rollbackErr != nil {
					return rollbackErr
				}
				refs := make([]corestore.WorkItemReference, 0, len(removals)+1)
				for _, removal := range removals {
					refs = append(refs, corestore.WorkItemReference{ProjectID: removal.RelatedProjectID, WorkItemID: removal.RelatedWorkItemID})
				}
				refs = append(refs, corestore.WorkItemReference{ProjectID: "prj_first", WorkItemID: "wi_blocker_2"})
				if err := w.RemoveOwnedDependencies(ctx, "prj_first", "wi_primary", "different-owner:", refs); err != nil {
					return err
				}
				var before int
				if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM issue_dependencies WHERE organization_id=$1`, organization).Scan(&before); err != nil || before != 201 {
					return fmt.Errorf("unmatched owner changed dependencies count=%d: %w", before, err)
				}
				changed, err := w.WorkItem(ctx, "prj_second", "wi_shared")
				if err != nil {
					return err
				}
				changed.NativeSourceKey = new("other-owner:changed")
				if changed, err = w.UpdateWorkItem(ctx, changed); err != nil {
					return err
				}
				refs = append(refs, refs[0])
				if err := w.RemoveOwnedDependencies(ctx, "prj_first", "wi_primary", "progress-check:", refs); err != nil {
					return err
				}
				retained, err := w.WorkItemBlockers(ctx, "prj_first", "wi_primary", corestore.WorkItemBlockerCursor{}, corestore.MaxPageSize)
				if err != nil || len(retained) != 200 {
					return fmt.Errorf("changed source ownership count=%d: %w", len(retained), err)
				}
				changed.NativeSourceKey = new("progress-check:restored")
				if changed, err = w.UpdateWorkItem(ctx, changed); err != nil {
					return err
				}
				if err := w.RemoveOwnedDependencies(ctx, "prj_first", "wi_primary", "progress-check:", refs); err != nil {
					return err
				}
				remaining, err := w.WorkItemBlockers(ctx, "prj_first", "wi_primary", corestore.WorkItemBlockerCursor{}, corestore.MaxPageSize)
				if err != nil || len(remaining) != 199 {
					return fmt.Errorf("owned dependency removal count=%d: %w", len(remaining), err)
				}
				for _, blocker := range remaining {
					if blocker.NativeSourceKey != nil && strings.HasPrefix(*blocker.NativeSourceKey, "progress-check:") {
						return errors.New("owned dependency was not removed")
					}
				}
				t.Logf("progress scope pages=%d blockers=%d owned=%d duration=%s", probe.pages, len(probe.seen), len(removals), time.Since(started))
				return nil
			})
		}()
		testenv.WaitUntil(t, t.Context(), func() error {
			var waiting bool
			if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND state='active' AND wait_event_type='Lock' AND query LIKE '-- name: LockRelationshipGraph :one%')`).Scan(&waiting); err != nil {
				return err
			}
			if !waiting {
				return errors.New("progress has not reached the graph lock")
			}
			return nil
		})
		probe, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var id string
		err = probe.QueryRowContext(t.Context(), `SELECT id FROM projects WHERE organization_id=$1 AND id='prj_first' FOR UPDATE NOWAIT`, organization).Scan(&id)
		rollbackErr = probe.Rollback()
		if err != nil {
			t.Fatalf("progress held a project while waiting for the graph: %v", err)
		}
		if rollbackErr != nil {
			t.Fatal(rollbackErr)
		}
		once.Do(func() { close(release) })
		if err := <-graphResult; err != nil {
			t.Fatal(err)
		}
		if err := <-progressResult; err != nil {
			t.Fatal(err)
		}
		cancel()
		tasks.Wait()
	}
}
