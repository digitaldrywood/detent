package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type importFixtureBackend struct {
	issueComment   bool
	incompleteBody bool
	failPage       bool
	omitDependency bool
	requests       []GitHubImportRequest
}

func (b *importFixtureBackend) FetchImportPage(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
	b.requests = append(b.requests, request)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	id := "I_issue"
	if request.IssueNumber == 2 {
		id = "I_blocker"
	}
	if request.IssueNumber > 2 {
		id = fmt.Sprintf("I_new_%d", request.IssueNumber)
	}
	comment := GitHubImportRecord{SourceKey: "comment:" + id, Kind: "comment", Data: json.RawMessage(`{"body":"Full historical comment"}`), Body: "Full historical comment", Provenance: tracker.Provenance{Provider: "github", ExternalID: "C_" + id, AuthorID: "U_author", AuthorDisplayName: "author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}
	switch request.Stage {
	case "issue":
		body := strings.Repeat("Complete body. ", 100) + issueContractTestSections
		if b.incompleteBody && request.IssueNumber == 2 {
			body = "## Roadmap Summary\nRepair the failed build."
		}
		page := GitHubImportPage{Issue: &IssueSource{NodeID: id, Number: request.IssueNumber, Title: "Imported issue", Body: body, URL: fmt.Sprintf("https://github.com/%s/issues/%d", request.Repository, request.IssueNumber), State: "open", AuthorID: "author", Labels: []string{"Todo"}, Assignees: []string{}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}, Records: []GitHubImportRecord{{SourceKey: "issue:" + id, Kind: "issue", Data: json.RawMessage(`{"id":"` + id + `"}`)}}, Gaps: []string{"Deleted comments and edit revisions are unavailable"}}
		if b.issueComment {
			page.Records = append(page.Records, comment)
		}
		return page, nil
	case "comments":
		if request.Cursor == "" {
			return GitHubImportPage{Records: []GitHubImportRecord{comment}, NextCursor: "page2"}, nil
		}
		if b.failPage {
			b.failPage = false
			return GitHubImportPage{}, errors.New("GitHub unavailable")
		}
		comment.SourceKey += "2"
		comment.Provenance.ExternalID += "2"
		comment.Body = "Last page comment"
		return GitHubImportPage{Records: []GitHubImportRecord{comment}}, nil
	case "timeline":
		return GitHubImportPage{Records: []GitHubImportRecord{comment, {SourceKey: "timeline:" + id, Kind: "timeline", Data: json.RawMessage(`{"event":"closed"}`)}}}, nil
	case "edits":
		return GitHubImportPage{}, nil
	case "dependencies":
		if request.IssueNumber != 1 || b.omitDependency {
			return GitHubImportPage{}, nil
		}
		return GitHubImportPage{Records: []GitHubImportRecord{{SourceKey: "dependency:I_blocker", Kind: "dependency", DependencyID: "I_blocker", Data: json.RawMessage(`{"node_id":"I_blocker"}`)}}}, nil
	default:
		return GitHubImportPage{}, errors.New("unexpected stage")
	}
}

func newIntegrationFixture(t *testing.T, backend ImportBackend) nativeFixture {
	t.Helper()
	s := newPostgresNativeService(t, Config{ImportBackend: backend})
	f := newNativeFixture(t, s, "", "import")
	f.token = testHubAdminToken
	if err := s.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Settings.Repository, project.Settings.CheckoutRepository = "example-org/example-project", "example-org/example-project"
		project.Settings.Intake, project.Settings.DuplicateChallenge = "manual", false
		project.States = hubrecords.NativeTriageStates(project.States)
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func startImportFixture(t *testing.T, f nativeFixture, number int, restart bool, revision tracker.Revision) GitHubImport {
	t.Helper()
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/imports", f.token, map[string]any{"idempotency_key": fmt.Sprintf("start-%d-%d", number, revision), "issue_number": number, "restart": restart, "expected_revision": fmt.Sprint(revision)})
	requireNativeStatus(t, r, http.StatusOK)
	var result GitHubImport
	decodeHubResponse(t, r, &result)
	return result
}

func advanceImportFixture(t *testing.T, f nativeFixture, job GitHubImport) GitHubImport {
	t.Helper()
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/imports/"+job.ID+"/advance", f.token, map[string]any{"expected_revision": fmt.Sprint(job.Revision)})
	requireNativeStatus(t, r, http.StatusOK)
	decodeHubResponse(t, r, &job)
	return job
}

func finishImportFixture(t *testing.T, f nativeFixture, job GitHubImport) GitHubImport {
	t.Helper()
	for range 20 {
		if job.Stage == "finished" {
			return job
		}
		job = advanceImportFixture(t, f, job)
	}
	t.Fatal("import did not finish within ten page advances")
	return job
}

func cutoverFixture(t *testing.T, f nativeFixture, request CutoverRequest) CutoverReceipt {
	t.Helper()
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/integration/cutover", f.token, request)
	requireNativeStatus(t, r, http.StatusOK)
	var result CutoverReceipt
	decodeHubResponse(t, r, &result)
	return result
}

func newCutoverIntegrationFixture(t *testing.T, backend ImportBackend) nativeFixture {
	t.Helper()
	f := newIntegrationFixture(t, backend)
	tx, err := hostedPostgresAdmin(t, f.service).BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, query := range []string{
		"DELETE FROM project_workflow_transitions WHERE organization_id=$1 AND project_id=$2",
		"DELETE FROM project_workflow_states WHERE organization_id=$1 AND project_id=$2",
		"UPDATE projects SET profile='github_compatible' WHERE organization_id=$1 AND id=$2",
	} {
		if _, err := tx.ExecContext(t.Context(), query, f.project.OrganizationID, f.project.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	f.project.Profile, f.project.States = "github_compatible", nil
	return f
}

func TestGitHubImportCheckpointCutoverAndNativeIsolation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		partial      bool
		incomplete   bool
		initial      string
		noPolicy     bool
		refused      bool
		issueComment bool
		legacy       bool
	}{
		{name: "complete", initial: "Todo"},
		{name: "issue page discussion", initial: "Todo", issueComment: true},
		{name: "unknown dependency fields", initial: "Todo", legacy: true},
		{name: "partial history", partial: true, initial: "Todo"},
		{name: "incomplete Todo", incomplete: true, initial: "Todo", refused: true},
		{name: "incomplete Backlog", incomplete: true, initial: "Backlog"},
		{name: "default contract without policy", initial: "Todo", noPolicy: true},
		{name: "incomplete default contract without policy", incomplete: true, initial: "Todo", noPolicy: true, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &importFixtureBackend{failPage: test.partial, incompleteBody: test.incomplete, issueComment: test.issueComment}
			f := newCutoverIntegrationFixture(t, backend)
			job := startImportFixture(t, f, 1, false, 0)
			job = advanceImportFixture(t, f, job)
			job = advanceImportFixture(t, f, job)
			stale := job.Revision
			job = advanceImportFixture(t, f, job)
			if test.partial {
				if job.Status != "partial" || job.Cursor != "page2" || job.Stage != "comments" || job.LastError == "" {
					t.Fatalf("partial checkpoint = %+v", job)
				}
				r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/imports/"+job.ID+"/advance", f.token, map[string]any{"expected_revision": fmt.Sprint(job.Revision)})
				requireNativeStatus(t, r, http.StatusTooManyRequests)
				var throttled GitHubImport
				decodeHubResponse(t, r, &throttled)
				if throttled.ID != job.ID || throttled.Revision != job.Revision || throttled.RetryAfter == nil {
					t.Fatalf("throttled import receipt = %+v", throttled)
				}
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE github_imports SET retry_after = NULL WHERE organization_id=$1 AND project_id=$2 AND id=$3", f.project.OrganizationID, f.project.ID, job.ID); err != nil {
					t.Fatal(err)
				}
			}
			r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/imports/"+job.ID+"/advance", f.token, map[string]any{"expected_revision": fmt.Sprint(stale)})
			requireNativeStatus(t, r, http.StatusConflict)
			job = finishImportFixture(t, f, job)
			if job.Status != "retrieved" || len(job.Gaps) == 0 {
				t.Fatalf("retrieval must retain limitations: %+v", job)
			}
			if test.legacy {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), `UPDATE github_import_records SET dependency_id='',provenance_external_id='' WHERE organization_id=$1 AND project_id=$2`, f.project.OrganizationID, f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			request := CutoverRequest{Mutation: tracker.Mutation{IdempotencyKey: "preview-unresolved"}, DryRun: true, InitialState: "Todo", States: []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}}}
			receipt := cutoverFixture(t, f, request)
			if receipt.Applied || receipt.UnresolvedDependencies != 1 || len(receipt.Blockers) == 0 {
				t.Fatalf("cutover must retain unresolved dependency: %+v", receipt)
			}
			second := finishImportFixture(t, f, startImportFixture(t, f, 2, false, 0))
			if test.initial == "Backlog" {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE issues SET workflow_state_name=NULL WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID); err != nil {
					t.Fatal(err)
				}
				request.States = append(request.States, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}})
			}
			request.InitialState = test.initial
			if test.noPolicy {
				clearNativeFixturePolicy(t, f)
			}
			request.IdempotencyKey = "preview-ready"
			receipt = cutoverFixture(t, f, request)
			if len(receipt.Blockers) != 0 || receipt.IncompleteImports != 0 {
				t.Fatalf("cutover preview = %+v", receipt)
			}
			request.DryRun, request.IdempotencyKey, request.Checkpoint = false, "apply", receipt.Checkpoint
			if test.refused {
				before := []tracker.NativeIssue{readWorkItem(t, f, tracker.NativeWorkItemID(job.WorkItemID), ""), readWorkItem(t, f, tracker.NativeWorkItemID(second.WorkItemID), "")}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/integration/cutover", f.token, request)
				requireNativeStatus(t, response, http.StatusUnprocessableEntity)
				for _, section := range []string{"Acceptance criteria", "Must not break", "How we know it worked", "Backlog/Triage"} {
					if !strings.Contains(response.Body.String(), section) {
						t.Errorf("cutover refusal missing %q: %s", section, response.Body)
					}
				}
				var integration ProjectIntegration
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/integration", f.token, nil), &integration)
				if integration.Profile != "github_compatible" || integration.Revision != receipt.Integration.Revision || integration.Intake != receipt.Integration.Intake || len(integration.States) != 0 {
					t.Fatalf("refused cutover changed integration: %+v", integration)
				}
				for _, original := range before {
					if got := readWorkItem(t, f, original.WorkItemID, ""); !reflect.DeepEqual(got, original) {
						t.Fatalf("refused cutover changed issue: %+v", got)
					}
				}
				for _, query := range []string{
					"SELECT count(*) FROM project_workflow_states WHERE organization_id=$1 AND project_id=$2",
					"SELECT count(*) FROM queue_entries WHERE organization_id=$1 AND project_id=$2",
					"SELECT count(*) FROM github_cutovers WHERE organization_id=$1 AND project_id=$2",
					"SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND type='issue.cutover'",
				} {
					var count int
					if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), query, f.project.OrganizationID, f.project.ID).Scan(&count); err != nil || count != 0 {
						t.Fatalf("refused cutover persisted rows for %q: %d, %v", query, count, err)
					}
				}
				return
			}
			receipt = cutoverFixture(t, f, request)
			if !receipt.Applied || receipt.Integration.Profile != "native" || receipt.Integration.Intake != "automatic" {
				t.Fatalf("cutover = %+v", receipt)
			}
			cutoverFixture(t, f, request)
			if test.initial == "Backlog" {
				for _, id := range []string{job.WorkItemID, second.WorkItemID} {
					if issue := readWorkItem(t, f, tracker.NativeWorkItemID(id), ""); issue.Profile != "native" || issue.State != "Backlog" {
						t.Fatalf("Backlog cutover issue = %+v", issue)
					}
				}
				return
			}
			path := f.base + "/work-items/" + job.WorkItemID
			r = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			var issue tracker.NativeIssue
			decodeHubResponse(t, r, &issue)
			if len(issue.Body) < 1000 || len(issue.Dependencies) != 1 {
				t.Fatalf("imported content/dependencies = %+v", issue)
			}
			if tracker.AppendGitHubIssueClosingReferences("Landing", issue.ExternalReferences) != "Landing\n\nCloses example-org/example-project#1" {
				t.Fatalf("imported issue lost closing identity: %+v", issue.ExternalReferences)
			}
			body := "Native body" + issueContractTestSections
			r = performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "native-edit"}, ExpectedRevision: issue.Revision, Body: &body})
			requireNativeStatus(t, r, http.StatusOK)
			f.create(t, "native creation")
			r = performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "native-comment"}, Body: "Native discussion"})
			requireNativeStatus(t, r, http.StatusOK)
			var count int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound").Scan(&count); err != nil || count != 0 {
				t.Fatalf("native writes queued GitHub mutations: %d, %v", count, err)
			}
			err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.(projectstore.GitHubIntakeReader).NextGitHubIntake(t.Context(), f.service.config.now())
				return err
			})
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("native idle polling target = %v", err)
			}
			r = performHubAPIRequest(t, f.service, http.MethodGet, "/api/v1/work-items/1", f.token, nil)
			requireNativeStatus(t, r, http.StatusNotFound)
			r = performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", f.token, map[string]any{"idempotency_key": "reimport-enable", "expected_revision": fmt.Sprint(receipt.Integration.Revision), "intake": "manual", "projection": "disabled", "repository_enabled": false})
			requireNativeStatus(t, r, http.StatusOK)
			job = finishImportFixture(t, f, startImportFixture(t, f, 1, true, job.Revision))
			r = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			decodeHubResponse(t, r, &issue)
			if issue.Body != body {
				t.Fatalf("reimport overwrote native body: %q", issue.Body)
			}
			r = performHubAPIRequest(t, f.service, http.MethodGet, path+"/comments", f.token, nil)
			var comments tracker.Page[tracker.NativeComment]
			decodeHubResponse(t, r, &comments)
			if len(comments.Items) != 3 {
				t.Fatalf("reimport duplicated comments: %+v", comments)
			}
			if test.issueComment {
				found := false
				for _, comment := range comments.Items {
					if comment.Provenance != nil && comment.Provenance.ExternalID == "C_I_issue" {
						found = true
						if comment.Sequence != 2 {
							t.Fatalf("issue-page comment sequence = %d, want 2", comment.Sequence)
						}
					}
				}
				if !found {
					t.Fatal("issue-page discussion was not retained")
				}
			}
			r = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/imports/"+job.ID+"/records?limit=2", f.token, nil)
			var records tracker.Page[GitHubImportRecord]
			decodeHubResponse(t, r, &records)
			if len(records.Items) != 2 || records.NextCursor == "" {
				t.Fatalf("source export pagination = %+v", records)
			}
		})
	}
}

func TestGitHubReimportDependencySnapshot(t *testing.T) {
	t.Parallel()
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained-%t", retained), func(t *testing.T) {
			backend := &importFixtureBackend{}
			f := newCutoverIntegrationFixture(t, backend)
			job := finishImportFixture(t, f, startImportFixture(t, f, 1, false, 0))
			if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), `UPDATE github_import_records SET current_dependency=false,dependency_id='',provenance_external_id='' WHERE organization_id=$1 AND project_id=$2 AND import_id=$3 AND kind='dependency'`, f.project.OrganizationID, f.project.ID, job.ID); err != nil {
				t.Fatal(err)
			}
			backend.omitDependency = !retained
			job = finishImportFixture(t, f, startImportFixture(t, f, 1, true, job.Revision))
			request := CutoverRequest{Mutation: tracker.Mutation{IdempotencyKey: "preview-reimport"}, DryRun: true, InitialState: "Todo", States: []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}}}
			receipt := cutoverFixture(t, f, request)
			want := 0
			if retained {
				want = 1
			}
			if receipt.UnresolvedDependencies != want {
				t.Fatalf("unresolved dependencies after reimport = %d, want %d", receipt.UnresolvedDependencies, want)
			}
			finishImportFixture(t, f, startImportFixture(t, f, 2, false, 0))
			request.IdempotencyKey = "preview-ready"
			receipt = cutoverFixture(t, f, request)
			request.DryRun, request.IdempotencyKey, request.Checkpoint = false, "apply", receipt.Checkpoint
			receipt = cutoverFixture(t, f, request)
			r := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+job.WorkItemID, f.token, nil)
			var issue tracker.NativeIssue
			decodeHubResponse(t, r, &issue)
			if len(issue.Dependencies) != want {
				t.Fatalf("cutover dependencies = %+v, want %d", issue.Dependencies, want)
			}
			var records int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM github_import_records WHERE organization_id=$1 AND project_id=$2 AND import_id=$3 AND kind='dependency'", f.project.OrganizationID, f.project.ID, job.ID).Scan(&records); err != nil || records != 1 {
				t.Fatalf("retained dependency history = %d, %v", records, err)
			}
			r = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/imports/"+job.ID+"/records", f.token, nil)
			var exported tracker.Page[GitHubImportRecord]
			decodeHubResponse(t, r, &exported)
			for _, record := range exported.Items {
				if record.Kind == "dependency" && (record.CurrentDependency == nil || *record.CurrentDependency != retained) {
					t.Fatalf("dependency export does not identify current membership: %+v", record)
				}
			}
			r = performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", f.token, map[string]any{"idempotency_key": "native-reimport", "expected_revision": fmt.Sprint(receipt.Integration.Revision), "intake": "manual", "projection": "disabled", "repository_enabled": false})
			requireNativeStatus(t, r, http.StatusOK)
			backend.omitDependency = retained
			finishImportFixture(t, f, startImportFixture(t, f, 1, true, job.Revision))
			r = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+job.WorkItemID, f.token, nil)
			decodeHubResponse(t, r, &issue)
			if len(issue.Dependencies) != want {
				t.Fatalf("source reimport changed native dependencies: %+v", issue.Dependencies)
			}
		})
	}
}

type cutoverStepBackend struct{ recordingOutboxBackend }

func (b *cutoverStepBackend) Step(ctx context.Context, item OutboxItem) (OutboxStepResult, error) {
	result := OutboxStepResult{Progress: item.Progress}
	if item.Uncertain {
		b.mu.Lock()
		b.verifyCalls++
		_, committed := b.seen[item.IdempotencyKey]
		b.mu.Unlock()
		if committed {
			result.Done = true
			return result, nil
		}
	}
	if err := item.BeforeWrite(ctx); err != nil {
		return result, err
	}
	err := b.Execute(ctx, item)
	result.Done = err == nil
	return result, err
}

func TestCutoverSourceClosureAndProjectionCoalescing(t *testing.T) {
	t.Parallel()
	for _, closeSource := range []bool{false, true} {
		t.Run(strconv.FormatBool(closeSource), func(t *testing.T) {
			f := newCutoverIntegrationFixture(t, &importFixtureBackend{})
			backend := &cutoverStepBackend{}
			f.service.outbox.stop()
			f.service.config.OutboxBackend = backend
			job := finishImportFixture(t, f, startImportFixture(t, f, 1, false, 0))
			finishImportFixture(t, f, startImportFixture(t, f, 2, false, 0))
			request := CutoverRequest{Mutation: tracker.Mutation{IdempotencyKey: "preview"}, DryRun: true, InitialState: "Todo", States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}, CloseSource: closeSource, DestinationURL: "https://detent.example/projects/native"}
			receipt := cutoverFixture(t, f, request)
			request.IdempotencyKey, request.DryRun, request.Checkpoint = "apply", false, receipt.Checkpoint
			receipt = cutoverFixture(t, f, request)
			var count int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE status = 'pending'").Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if closeSource {
				want = 2
			}
			if count != want {
				t.Fatalf("source closure count = %d, want %d", count, want)
			}
			for range want {
				if _, err := f.service.ProcessOutbox(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			r := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", f.token, map[string]any{"idempotency_key": "summaries", "expected_revision": fmt.Sprint(receipt.Integration.Revision), "intake": "disabled", "projection": "summary", "repository_enabled": false})
			requireNativeStatus(t, r, http.StatusOK)
			for _, key := range []string{"first", "last", "last"} {
				r = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+job.WorkItemID+"/projection", f.token, map[string]any{"idempotency_key": key, "body": key + " summary"})
				requireNativeStatus(t, r, http.StatusOK)
			}
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE status = 'pending'").Scan(&count); err != nil || count != 1 {
				t.Fatalf("coalesced count = %d: %v", count, err)
			}
			backend.failures = []error{errors.New("temporary outage")}
			if _, err := f.service.ProcessOutbox(t.Context()); err == nil {
				t.Fatal("missing projection failure")
			}
			if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE github_outbound SET next_attempt_at = CURRENT_TIMESTAMP - interval '1 second' WHERE status = 'retrying'"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.ProcessOutbox(t.Context()); err != nil {
				t.Fatal(err)
			}
			if backend.effects != want+1 {
				t.Fatalf("projection effects = %d", backend.effects)
			}
			r = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+job.WorkItemID+"/projection", f.token, map[string]any{"idempotency_key": "uncertain", "body": "Uncertain prior summary"})
			requireNativeStatus(t, r, http.StatusOK)
			backend.failures = []error{errors.New("publication outcome is unknown")}
			if _, err := f.service.ProcessOutbox(t.Context()); err == nil {
				t.Fatal("missing uncertain projection failure")
			}
			r = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+job.WorkItemID+"/projection", f.token, map[string]any{"idempotency_key": "newest", "body": "Newest summary"})
			requireNativeStatus(t, r, http.StatusOK)
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND status='retrying' AND outcome='uncertain'", f.project.OrganizationID, f.project.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("uncertain publication custody = %d, %v", count, err)
			}
			if worked, err := f.service.ProcessOutbox(t.Context()); err != nil || worked {
				t.Fatalf("newer summary passed uncertain prior publication: %t, %v", worked, err)
			}
			if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE github_outbound SET next_attempt_at=$3 WHERE organization_id=$1 AND project_id=$2 AND status='retrying'", f.project.OrganizationID, f.project.ID, f.service.config.now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if worked, err := f.service.ProcessOutbox(t.Context()); err != nil || !worked {
					t.Fatalf("summary reconciliation did not progress: %t, %v", worked, err)
				}
			}
			if backend.effects != want+3 {
				t.Fatalf("reconciled summary effects = %d, want %d", backend.effects, want+3)
			}
			keys := backend.keys()
			if len(keys) < 2 || !strings.HasSuffix(keys[len(keys)-1], ":newest") || !strings.HasSuffix(keys[len(keys)-2], ":uncertain") {
				t.Fatalf("summary publication order = %v", keys)
			}
			_, secondToken := cutoverCredentialFixture(t, f)
			for _, command := range []struct{ token, body string }{{f.token, "First operator summary"}, {secondToken, "Second operator summary"}, {secondToken, "Second operator summary"}} {
				r = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+job.WorkItemID+"/projection", command.token, map[string]any{"idempotency_key": "shared-key", "body": command.body})
				requireNativeStatus(t, r, http.StatusOK)
			}
			var pending projectstore.GitHubOutbound
			var pendingKey string
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT command_key FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND status='pending'", f.project.OrganizationID, f.project.ID).Scan(&pendingKey); err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				pending, err = r.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), pendingKey)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			raw, err := eventstore.NewContent(f.service.runDetailStore()).Receipt(t.Context(), pending.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "Second operator summary") || strings.Contains(string(raw), "First operator summary") {
				t.Fatalf("second credential summary was lost: %s", raw)
			}
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND coalesce_key=$3 AND status IN ('pending','retrying','processing')", f.project.OrganizationID, f.project.ID, pending.CoalesceKey).Scan(&count); err != nil || count != 1 {
				t.Fatalf("two-credential summary publication count = %d, %v", count, err)
			}
			if worked, err := f.service.ProcessOutbox(t.Context()); err != nil || !worked {
				t.Fatalf("second credential summary was not published: %t, %v", worked, err)
			}
			if backend.effects != want+4 {
				t.Fatalf("second credential publication effects = %d, want %d", backend.effects, want+4)
			}
		})
	}
}

func cutoverCredentialFixture(t *testing.T, f nativeFixture) (string, string) {
	t.Helper()
	id, token := hubapi.NewID("credential"), hubapi.NewID("token")
	now := f.service.config.now()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: id, Name: "Example scoped administrator", Hash: apikey.HashToken(token), Scope: "admin", KeyScope: "admin", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: id, ProjectID: f.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	return id, token
}

func cutoverOwnedRows(t *testing.T, f nativeFixture) map[string][]string {
	t.Helper()
	result := map[string][]string{}
	for _, table := range []string{"projects", "project_settings", "project_workflow_states", "project_workflow_transitions", "project_policies", "issues", "issue_claim_facts", "queue_entries", "issue_dependencies", "collaboration_versions", "collaboration_events", "search_documents", "github_cutovers", "github_outbound", "native_commands", "native_command_states", "native_board_sequences", "native_board_deltas", "native_board_dirty"} {
		rows, err := hostedPostgresAdmin(t, f.service).QueryContext(t.Context(), "SELECT * FROM "+table+" WHERE organization_id=$1", f.project.OrganizationID)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		values, pointers := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		records := []string{}
		for rows.Next() {
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			raw, err := json.Marshal(values)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			records = append(records, string(raw))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(records)
		result[table] = records
	}
	return result
}

func TestCutoverRefusesChangedAuthoritySourcesAndRollsBack(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"foreign project", "foreign organization", "revoked replay", "changed checkpoint", "changed policy", "changed source", "uncertain compatibility write", "ambiguous queue positions", "partial dependency history", "mid-commit fault"} {
		t.Run(name, func(t *testing.T) {
			f := newCutoverIntegrationFixture(t, &importFixtureBackend{})
			first := finishImportFixture(t, f, startImportFixture(t, f, 1, false, 0))
			finishImportFixture(t, f, startImportFixture(t, f, 2, false, 0))
			id, token := cutoverCredentialFixture(t, f)
			f.token = token
			request := CutoverRequest{Mutation: tracker.Mutation{IdempotencyKey: "reviewed-preview"}, DryRun: true, InitialState: "Todo", States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}
			reviewed := cutoverFixture(t, f, request)
			target := f.base + "/integration/cutover"
			status := http.StatusConflict
			if name != "revoked replay" {
				request.IdempotencyKey, request.DryRun, request.Checkpoint = "apply-reviewed", false, reviewed.Checkpoint
			}
			switch name {
			case "foreign project":
				other := newNativeFixture(t, f.service, f.project.OrganizationID, "Other project")
				target, status = other.base+"/integration/cutover", http.StatusNotFound
			case "foreign organization":
				other := newNativeFixture(t, f.service, f.project.OrganizationID+"_second", "Other organization")
				target, status = other.base+"/integration/cutover", http.StatusUnauthorized
			case "revoked replay":
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=$3 WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, id, f.service.config.now()); err != nil {
					t.Fatal(err)
				}
				status = http.StatusUnauthorized
			case "changed checkpoint":
				request.Checkpoint = strings.Repeat("a", 64)
			case "changed policy":
				clearNativeFixturePolicy(t, f)
			case "changed source":
				finishImportFixture(t, f, startImportFixture(t, f, 1, true, first.Revision))
			case "uncertain compatibility write":
				payload, err := eventstore.NewContent(f.service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID}, []byte(`{"state":"Todo"}`))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					owner := w.(projectstore.GitHubOutboxWriter)
					now := f.service.config.now()
					if err := owner.EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: f.project.ID, WorkItemID: tracker.NativeWorkItemID(first.WorkItemID), Key: "uncertain-source", Repository: "example-org/example-project", IssueNumber: 1, Kind: string(MutationWorkflowLabel), CreatedAt: now, Payload: payload}); err != nil {
						return err
					}
					row, err := owner.ClaimGitHubOutbound(t.Context(), now, now.Add(time.Minute), "uncertain-source-lease")
					if err != nil {
						return err
					}
					if err := owner.RenewGitHubOutbound(t.Context(), row.ID, row.LeaseID, now, now.Add(time.Minute), true); err != nil {
						return err
					}
					row.Status, row.Outcome, row.NextAttemptAt = "retrying", "uncertain", now.Add(time.Minute)
					return owner.FinishGitHubOutbound(t.Context(), row, now)
				}); err != nil {
					t.Fatal(err)
				}
				status = http.StatusUnprocessableEntity
			case "ambiguous queue positions":
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					for _, scope := range []string{"source-first", "source-second"} {
						if err := w.PutQueueEntry(t.Context(), claimstore.QueueEntry{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: tracker.NativeWorkItemID(first.WorkItemID), Scope: scope, State: "Todo", Rank: scope, CreatedAt: f.service.config.now(), UpdatedAt: f.service.config.now()}); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				status = http.StatusUnprocessableEntity
			case "partial dependency history":
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					owner := w.(projectstore.GitHubImportWriter)
					row, err := owner.GitHubImport(t.Context(), f.project.ID, first.ID)
					if err != nil {
						return err
					}
					row.Stage, row.Status, row.Cursor = "dependencies", "partial", "more-dependencies"
					_, err = owner.UpdateGitHubImport(t.Context(), row)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				request.DryRun, request.AcceptPartial, request.IdempotencyKey, request.Checkpoint = true, true, "partial-dependency-preview", ""
				partial := cutoverFixture(t, f, request)
				if len(partial.Blockers) == 0 {
					t.Fatal("partial history acceptance omitted incomplete dependency pages")
				}
				request.DryRun, request.IdempotencyKey, request.Checkpoint = false, "apply-partial-dependencies", partial.Checkpoint
				status = http.StatusUnprocessableEntity
			case "mid-commit fault":
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "CREATE SEQUENCE test_cutover_fault_seen"); err != nil {
					t.Fatal(err)
				}
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "GRANT USAGE ON SEQUENCE test_cutover_fault_seen TO PUBLIC"); err != nil {
					t.Fatal(err)
				}
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), `CREATE FUNCTION test_cutover_event_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.type='issue.cutover' THEN PERFORM nextval('test_cutover_fault_seen'); RAISE EXCEPTION 'cutover event fault'; END IF; RETURN NEW; END $$`); err != nil {
					t.Fatal(err)
				}
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), `CREATE TRIGGER test_cutover_event_failure BEFORE INSERT ON collaboration_events FOR EACH ROW EXECUTE FUNCTION test_cutover_event_failure()`); err != nil {
					t.Fatal(err)
				}
				status = http.StatusInternalServerError
			}
			before := cutoverOwnedRows(t, f)
			response := performHubAPIRequest(t, f.service, http.MethodPost, target, token, request)
			requireNativeStatus(t, response, status)
			after := cutoverOwnedRows(t, f)
			for table, original := range before {
				if !reflect.DeepEqual(after[table], original) {
					t.Errorf("refused cutover changed %s rows", table)
				}
			}
			if name == "mid-commit fault" {
				var reached bool
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT is_called FROM test_cutover_fault_seen").Scan(&reached); err != nil || !reached {
					t.Fatalf("cutover fault was not reached: %t, %v", reached, err)
				}
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "DROP TRIGGER test_cutover_event_failure ON collaboration_events"); err != nil {
					t.Fatal(err)
				}
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "DROP FUNCTION test_cutover_event_failure()"); err != nil {
					t.Fatal(err)
				}
				if retried := cutoverFixture(t, f, request); !retried.Applied {
					t.Fatal("rolled-back command could not be retried")
				}
			}
		})
	}
}

type integrationRevokingRepository struct {
	*postgres.Repository
	revoke atomic.Pointer[func(context.Context) error]
}

type integrationProjectReadSpy struct {
	tenantstore.Reader
	read *bool
}

func (r integrationProjectReadSpy) ProjectRecord(ctx context.Context, project tracker.ProjectID) (projectstore.Record, error) {
	*r.read = true
	return r.Reader.ProjectRecord(ctx, project)
}

func (r *integrationRevokingRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	if r.revoke.Load() == nil {
		return r.Repository.ReadTenant(ctx, organization, workload, read)
	}
	var projectRead bool
	err := r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(integrationProjectReadSpy{Reader: reader, read: &projectRead})
	})
	if err == nil && projectRead {
		if revoke := r.revoke.Swap(nil); revoke != nil {
			err = (*revoke)(ctx)
		}
	}
	return err
}

func TestProjectIntegrationToolRechecksAuthorityBeforeLiveRead(t *testing.T) {
	t.Parallel()
	postgresRepository, _ := projectRecordPostgresWithAdmin(t, 3)
	repository := &integrationRevokingRepository{Repository: postgresRepository}
	now := time.Now().UTC().Truncate(time.Microsecond)
	var live atomic.Int32
	service := &Service{echo: echo.New(), database: &database{postgres: postgresRepository}, secretRewrapReady: make(chan struct{}), config: Config{Logger: discardLogger(), TenantRepository: repository, AccessRepository: postgresRepository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now },
		ReconcileBackend: &gitHubRepairFixture{},
		GitHubAppInstallation: func(context.Context, string) (GitHubAppInstallation, error) {
			live.Add(1)
			return GitHubAppInstallation{Slug: "example-app", Installed: true}, nil
		}}}
	service.registerNativeRoutes(service.echo)
	service.registerOperatorTools(service.echo)
	service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{service}, chat.WithClock(service.config.now))
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for _, org := range organizations {
		seedStartupOrganization(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "reader_" + string(org), Name: "Example reader", Hash: apikey.HashToken(string(org)), Scope: "operator", KeyScope: "read", ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			created, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: tracker.ProjectID("project_" + string(org)), Name: "Example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			created.Settings.Repository = "example-org/example-project"
			if _, err := w.UpdateProjectRecord(t.Context(), created); err != nil {
				return err
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "reader_" + string(org), ProjectID: tracker.ProjectID("project_" + string(org))})
		}); err != nil {
			t.Fatal(err)
		}
	}
	call := func(org tracker.OrganizationID, project string) *httptest.ResponseRecorder {
		service.localOrganization = string(org)
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "get_project_integration", "arguments": map[string]any{"project_id": project}, "_meta": json.RawMessage(fleetProtocolMeta)}}
		return performHubWorkCall(t, service, "/api/v2/organizations/"+string(org)+"/mcp", string(org), "get_project_integration", body)
	}
	type toolResult struct {
		Result struct {
			IsError bool `json:"isError"`
			Data    struct {
				Integration ProjectIntegration `json:"data"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	for _, org := range organizations {
		requireNativeStatus(t, call(org, "project_"+string(org)), http.StatusOK)
	}
	if calls := live.Load(); calls != int32(len(organizations)) {
		t.Fatalf("authorized live callback calls = %d, want %d", calls, len(organizations))
	}
	revoked, other := organizations[0], organizations[1]
	project := "project_" + string(revoked)
	live.Store(0)
	revoke := func(ctx context.Context) error {
		return postgresRepository.WriteTenant(context.WithoutCancel(ctx), revoked, corestore.Serving, func(w tenantstore.Writer) error {
			return w.DeleteAccessTokenGrant(ctx, "reader_"+string(revoked), tracker.ProjectID(project))
		})
	}
	repository.revoke.Store(&revoke)
	response := call(revoked, project)
	if repository.revoke.Load() != nil {
		t.Fatal("authority was not revoked between the project read and the live read")
	}
	if calls := live.Load(); calls != 0 {
		t.Fatalf("live callback ran %d times for a revoked caller: %s", calls, response.Body.String())
	}
	var revokedResult toolResult
	decodeHubResponse(t, response, &revokedResult)
	if !revokedResult.Result.IsError && response.Code == http.StatusOK {
		t.Fatalf("revoked caller received the integration: %s", response.Body.String())
	}
	fresh := call(revoked, project)
	missing := call(revoked, "project_missing")
	for name, answer := range map[string]*httptest.ResponseRecorder{"fresh hidden project": fresh, "missing project": missing} {
		if answer.Code != response.Code || answer.Body.String() != response.Body.String() {
			t.Errorf("revoked live read answer %d %s differs from the %s answer %d %s", response.Code, response.Body.String(), name, answer.Code, answer.Body.String())
		}
	}
	if calls := live.Load(); calls != 0 {
		t.Fatalf("live callback ran %d times for a hidden project", calls)
	}
	authorized := call(other, "project_"+string(other))
	requireNativeStatus(t, authorized, http.StatusOK)
	var result toolResult
	decodeHubResponse(t, authorized, &result)
	if result.Result.IsError || result.Result.Data.Integration.GitHubAppInstalled == nil || !*result.Result.Data.Integration.GitHubAppInstalled {
		t.Fatalf("authorized tenant integration = %s", authorized.Body.String())
	}
	if calls := live.Load(); calls != 1 {
		t.Fatalf("authorized tenant %s live callback calls = %d, want 1", other, calls)
	}
}
