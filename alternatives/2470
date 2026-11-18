package hubserver

import (
	"context"
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

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		writer := &progressLeaseOrderWriter{issue: make(chan struct{}, 1), lease: make(chan struct{}, 1), issueWait: make(chan struct{})}
		writer.lease <- struct{}{}
		owner := nativeProgressOwner{service: &Service{config: Config{Clock: time.Now}}, scope: hubscope.NativeScope{Credential: hubapi.APICredential{Scope: hubapi.APIScopeWorker}}}
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
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return clock.Add(time.Duration(offset.Load())) }})
	scope := nativeBoardFixtureScope(t, f)
	readRelationships := func(t *testing.T, id tracker.NativeWorkItemID) tracker.NativeIssue {
		t.Helper()
		issue := readWorkItem(t, f, id, "")
		if err := f.service.hydrateWorkItemRelationshipFacts(t.Context(), scope, &issue); err != nil {
			t.Fatal(err)
		}
		return issue
	}
	admin := conversationTestAdmin(t, f.service)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), scope.Project)
		if err != nil {
			return err
		}
		project.States = append(project.States, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}})
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	runner.enroll(t)
	readRequest := func(t *testing.T, issue tracker.NativeIssue) (progresscheck.Request, int64) {
		t.Helper()
		request, revision, err := f.service.nativeProgressRequestTenant(t.Context(), scope, string(issue.WorkItemID), hubTestPolicy().ID, string(runner.binding.MachineID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := request.ID(); err != nil {
			t.Fatal(err)
		}
		return request, revision
	}
	seedAttempts := func(t *testing.T, issue tracker.NativeIssue, key string) progresscheck.Request {
		t.Helper()
		path := f.base + "/work-items/" + string(issue.WorkItemID)
		for index := range 2 {
			lease := nativeAttemptFixtureLease(t, runner, issue, fmt.Sprintf("%s-%d", key, index))
			start := nativeStartedEvent(lease)
			start.IdempotencyKey = fmt.Sprintf("%s-start-%d", key, index)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", runner.redemption.Credential, start), http.StatusOK)
			finish := start
			finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", fmt.Sprintf("%s-finish-%d", key, index), 2, "failed"
			finish.Data.TerminalFailure = &tracker.NativeTerminalFailure{Stage: "implement", Attribution: "instance", ErrorClass: "protocol", Error: fmt.Sprintf("backend protocol unavailable: request-%s-%d", key, index), ObservedAt: f.service.config.Clock(), Source: "backend", Coverage: "complete", Summary: "Example protocol refusal"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", runner.redemption.Credential, finish), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		}
		request, _ := readRequest(t, issue)
		return request
	}
	appendHistory := func(t *testing.T, issue tracker.NativeIssue, eventType string) {
		t.Helper()
		raw, err := json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: issue.WorkItemID, AggregateType: "issue", Type: eventType, SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: f.service.config.Clock()})
		if err != nil {
			t.Fatal(err)
		}
		event, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), scope.Organization, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
			t.Fatal(err)
		}
	}
	var previousIssue string
	for _, outcome := range []progresscheck.Outcome{progresscheck.Progressing, progresscheck.Reroute, progresscheck.SystemDefect} {
		t.Run(string(outcome), func(t *testing.T) {
			issue := f.createIndependent(t, "Example "+string(outcome))
			request := seedAttempts(t, issue, string(outcome))
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			decision := progresscheck.Decision{Outcome: outcome, Diagnosis: request.Attempts[len(request.Attempts)-1].ID + ": The retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the source finalization check and retain its command receipt", Convergence: "The previously failing check has a successful receipt on the repaired assertion", Reason: "Source or evidence requires another step", Evidence: []string{request.Attempts[len(request.Attempts)-1].ID}}
			if outcome == progresscheck.Reroute {
				decision.Step = "implement"
			}
			if outcome == progresscheck.SystemDefect {
				decision.DefectKey, decision.DefectTitle, decision.DefectBody = "example-protocol", "Example protocol refusal", issueContractTestSections
			}
			owner := nativeProgressOwner{service: f.service, scope: scope, request: request}
			record := progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: decision}
			_, revision := readRequest(t, issue)
			owner.guardRevision = &revision
			title := issue.Title + " updated source"
			response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "stale-progress-" + string(outcome)}, ExpectedRevision: issue.Revision, Title: &title})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
			if _, err := owner.Publish(t.Context(), record); err == nil {
				t.Fatal("new source evidence was accepted under the old publication revision")
			}
			request, _ = readRequest(t, issue)
			id, err = request.ID()
			if err != nil {
				t.Fatal(err)
			}
			record.RequestID, record.EvidenceRevision = id, request.EvidenceRevision
			owner.request = request
			for _, eventType := range []string{"run.started", "run.observed", "workflow.transitioned", "run.finished", "run.checkpointed"} {
				_, revision = readRequest(t, issue)
				owner.guardRevision = &revision
				appendHistory(t, issue, eventType)
				if _, err := owner.Publish(t.Context(), record); err == nil {
					t.Fatalf("%s allowed a publication against stale canonical event sequence", eventType)
				}
			}
			request, _ = readRequest(t, issue)
			id, err = request.ID()
			if err != nil {
				t.Fatal(err)
			}
			record.RequestID, record.EvidenceRevision = id, request.EvidenceRevision
			owner.request = request
			owner.guardRevision = nil
			if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION reject_progress_receipt_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.actor_id='native-progress' THEN RAISE EXCEPTION 'injected final progress persistence failure'; END IF; RETURN NEW; END $$`); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `CREATE TRIGGER reject_progress_receipt BEFORE INSERT ON native_commands FOR EACH ROW EXECUTE FUNCTION reject_progress_receipt_write()`); err != nil {
				t.Fatal(err)
			}
			_, publishErr := owner.Publish(t.Context(), record)
			if _, err := admin.ExecContext(t.Context(), `DROP TRIGGER reject_progress_receipt ON native_commands`); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `DROP FUNCTION reject_progress_receipt_write()`); err != nil {
				t.Fatal(err)
			}
			if publishErr == nil {
				t.Fatal("injected final storage failure accepted")
			}
			for _, query := range []string{
				`SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`,
				`SELECT count(*) FROM native_commands WHERE organization_id=$1 AND actor_id='native-progress' AND operation='native progress/'||$2||'/'||$3`,
			} {
				var count int
				if err := admin.QueryRowContext(t.Context(), query, scope.Organization, scope.Project, issue.WorkItemID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("publication failure retained %d rows", count)
				}
			}
			if outcome == progresscheck.SystemDefect {
				var count int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2 AND title=$3`, scope.Organization, scope.Project, decision.DefectTitle).Scan(&count); err != nil {
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
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, scope.Organization, scope.Project, issue.WorkItemID).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if comments != 1 {
				t.Fatalf("replay published %d comments", comments)
			}
			if previousIssue != "" {
				other := owner
				other.request.IssueID = previousIssue
				_, found, err := other.Load(t.Context(), request.ProjectID, previousIssue, id)
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
			dependent := readRelationships(t, issue.WorkItemID)
			if len(dependent.Dependencies) != 1 || string(dependent.Dependencies[0]) != published.DefectIssueID || dependent.DependencyReady {
				t.Fatalf("progress defect has no dispatch dependency: %+v", dependent)
			}
			page, err := f.service.readCommentRecords(t.Context(), scope, request.IssueID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 {
				t.Fatalf("progress comment count=%d", len(page.Items))
			}
			defectPath, err := f.service.nativeIssueRecordBrowserPath(t.Context(), scope, published.DefectIssueID)
			if err != nil || defectPath == "" {
				t.Fatalf("defect path=%q error=%v", defectPath, err)
			}
			if !strings.Contains(page.Items[0].Body, "]("+defectPath+")") {
				t.Fatalf("missing defect link: comment=%q", page.Items[0].Body)
			}
			refresh := func() {
				t.Helper()
				request, _ = readRequest(t, issue)
				id, err = request.ID()
				if err != nil {
					t.Fatal(err)
				}
				record.RequestID, record.EvidenceRevision = id, request.EvidenceRevision
				record.CommentID, record.DefectIssueID = "", ""
				owner.request = request
			}
			human := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+request.IssueID+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "new-human-input"}, Body: "New human input"})
			requireNativeStatus(t, human, http.StatusOK)
			var humanComment tracker.NativeComment
			decodeHubResponse(t, human, &humanComment)
			refresh()
			second, err := owner.Publish(t.Context(), record)
			if err != nil || second.DefectIssueID != published.DefectIssueID {
				t.Fatalf("defect deduplication = %+v, %v", second, err)
			}
			t.Run("shared outage across distinct issues creates one defect", func(t *testing.T) {
				for index := range 6 {
					other := f.createIndependent(t, fmt.Sprintf("Outage issue %d", index))
					outage := seedAttempts(t, other, fmt.Sprintf("outage-%d", index))
					outageDecision := decision
					outageDecision.Evidence = []string{outage.Attempts[len(outage.Attempts)-1].ID}
					outageDecision.Diagnosis = outageDecision.Evidence[0] + ": The retained protocol failure identifies an instance defect requiring the shared repair"
					backend := sharedOutageProgressBackend{decision: outageDecision}
					otherOwner := nativeProgressOwner{service: f.service, scope: scope, request: outage}
					result, checked, err := progresscheck.Check(t.Context(), outage, backend, otherOwner)
					if err != nil || !checked || result.DefectIssueID != published.DefectIssueID || result.CommentID == published.CommentID {
						t.Fatalf("issue %s did not reuse the shared defect: result=%+v checked=%t error=%v", other.WorkItemID, result, checked, err)
					}
					linked := readRelationships(t, other.WorkItemID)
					if len(linked.Dependencies) != 1 || string(linked.Dependencies[0]) != published.DefectIssueID || linked.DependencyReady {
						t.Fatalf("issue %s lost its shared repair dependency: %+v", other.WorkItemID, linked)
					}
				}
				var defects int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2 AND title=$3`, scope.Organization, scope.Project, decision.DefectTitle).Scan(&defects); err != nil {
					t.Fatal(err)
				}
				if defects != 1 {
					t.Fatalf("shared outage created %d defect records, want 1", defects)
				}
			})
			offset.Store(int64(100 * time.Millisecond))
			edited := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+request.IssueID+"/comments/"+humanComment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "different-defect"}, ExpectedRevision: humanComment.Revision, Body: "Different defect evidence"})
			requireNativeStatus(t, edited, http.StatusOK)
			decodeHubResponse(t, edited, &humanComment)
			refresh()
			record.Decision.DefectKey = "different-protocol"
			latest, err := owner.Publish(t.Context(), record)
			if err != nil {
				t.Fatal(err)
			}
			latestIssue := readWorkItem(t, f, tracker.NativeWorkItemID(latest.DefectIssueID), "")
			if latest.DefectIssueID == published.DefectIssueID || latestIssue.State != "Backlog" {
				t.Fatalf("latest defect=%s state=%s", latest.DefectIssueID, latestIssue.State)
			}
			for _, eventType := range []string{"comment.created", "comment.edited", "change.version_published", "run.checkpointed", "issue.edited"} {
				edited := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+request.IssueID+"/comments/"+humanComment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "input-before-" + eventType}, ExpectedRevision: humanComment.Revision, Body: "New source before " + eventType})
				requireNativeStatus(t, edited, http.StatusOK)
				decodeHubResponse(t, edited, &humanComment)
				refresh()
				if _, err := owner.Publish(t.Context(), record); err != nil {
					t.Fatal(err)
				}
				linked := readRelationships(t, issue.WorkItemID)
				if linked.DependencyReady || len(linked.Dependencies) != 1 {
					t.Fatalf("missing dependency before %s", eventType)
				}
				switch eventType {
				case "comment.created":
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+request.IssueID+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "release-progress-comment"}, Body: "New human input after progress"})
					requireNativeStatus(t, response, http.StatusOK)
				case "comment.edited":
					response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+request.IssueID+"/comments/"+humanComment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "release-progress-edited"}, ExpectedRevision: humanComment.Revision, Body: "Edited human input after progress"})
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &humanComment)
				case "issue.edited":
					current := readWorkItem(t, f, issue.WorkItemID, "")
					title := current.Title + " new human evidence"
					response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+request.IssueID, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "release-progress-edit"}, ExpectedRevision: current.Revision, Title: &title})
					requireNativeStatus(t, response, http.StatusOK)
				default:
					appendHistory(t, issue, eventType)
				}
				reopened := readRelationships(t, issue.WorkItemID)
				if !reopened.DependencyReady || len(reopened.Dependencies) != 0 {
					t.Fatalf("%s retained progress veto", eventType)
				}
			}
			refresh()
			if _, err := owner.Publish(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			defect := readWorkItem(t, f, tracker.NativeWorkItemID(latest.DefectIssueID), "")
			for _, state := range []string{"Todo", "Done"} {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+latest.DefectIssueID+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "defect-" + state}, ExpectedRevision: defect.Revision, State: state, Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &defect)
			}
			reopened := readRelationships(t, issue.WorkItemID)
			if !reopened.DependencyReady {
				t.Fatal("landed defect still vetoes dispatch")
			}
			ordinary := f.createIndependent(t, "Ordinary dependency")
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+request.IssueID+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "ordinary-dependency"}, ExpectedRevision: reopened.Revision, Operation: "add", RelatedWorkItemID: ordinary.WorkItemID})
			requireNativeStatus(t, response, http.StatusOK)
			appendHistory(t, issue, "issue.edited")
			retained := readRelationships(t, issue.WorkItemID)
			if retained.DependencyReady || len(retained.Dependencies) != 1 || retained.Dependencies[0] != ordinary.WorkItemID {
				t.Fatal("new evidence changed ordinary dependency")
			}
		})
	}
}

func TestNativeProgressRetrievedEvidenceIsScoped(t *testing.T) {
	t.Parallel()
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
			if test.source == "same" {
				for index := range progresscheck.MaxComments {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("recent-comment-%d", index)}, Body: "Newer context"})
					requireNativeStatus(t, response, http.StatusOK)
				}
			}
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Events)
			runner.enroll(t)
			lease := nativeAttemptFixtureLease(t, runner, issue, "retrieved-evidence")
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			event := nativeStartedEvent(lease)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "retrieved-checkpoint", 2
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.HeadSHA = strings.Repeat("a", 40)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome, event.Data.Handoff = "run.finished", "retrieved-finish", 3, "failed", nil
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, event), http.StatusOK)
			request, _, err := f.service.nativeProgressRequestTenant(t.Context(), analyticsTestScope(t, f), string(issue.WorkItemID), hubTestPolicy().ID, string(runner.binding.MachineID))
			if err != nil {
				t.Fatal(err)
			}
			var sourceReference progresscheck.EvidenceReference
			if strings.HasPrefix(test.source, "source") {
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
			owner := nativeProgressOwner{service: f.service, scope: analyticsTestScope(t, f), request: request}
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
	t.Parallel()
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
				provenance := "native"
				if source != nil {
					provenance = "progress_check"
				}
				if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: organization, ProjectID: "prj_first", WorkItemID: "wi_primary", RelatedProjectID: project, RelatedWorkItemID: id, Type: "dependency", Provenance: provenance}, "add", time.Now().UTC()); err != nil {
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
		var updateLock *pgconn.PgError
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
		owner := nativeProgressOwner{scope: hubscope.NativeScope{Organization: organization, Project: "prj_first"}}
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
					var locked *pgconn.PgError
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
