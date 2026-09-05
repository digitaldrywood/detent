package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type importFixtureBackend struct {
	failPage bool
	requests []GitHubImportRequest
}

func (b *importFixtureBackend) FetchImportPage(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
	b.requests = append(b.requests, request)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	id := "I_issue"
	if request.IssueNumber == 2 {
		id = "I_blocker"
	}
	if request.IssueNumber > 2 {
		id = "I_new"
	}
	comment := GitHubImportRecord{SourceKey: "comment:" + id, Kind: "comment", Data: json.RawMessage(`{"body":"Full historical comment"}`), Body: "Full historical comment", Provenance: tracker.Provenance{Provider: "github", ExternalID: "C_" + id, AuthorID: "U_author", AuthorDisplayName: "author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}
	switch request.Stage {
	case "issue":
		return GitHubImportPage{Issue: &IssueSource{NodeID: id, Number: request.IssueNumber, Title: "Imported issue", Body: strings.Repeat("Complete body. ", 100), URL: "https://github.com/digitaldrywood/detent/issues/1", State: "open", AuthorID: "author", Labels: []string{"Todo"}, Assignees: []string{}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}, Records: []GitHubImportRecord{{SourceKey: "issue:" + id, Kind: "issue", Data: json.RawMessage(`{"id":"` + id + `"}`)}}, Gaps: []string{"Deleted comments and edit revisions are unavailable"}}, nil
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
		if request.IssueNumber != 1 {
			return GitHubImportPage{}, nil
		}
		return GitHubImportPage{Records: []GitHubImportRecord{{SourceKey: "dependency:I_blocker", Kind: "dependency", DependencyID: "I_blocker", Data: json.RawMessage(`{"node_id":"I_blocker"}`)}}}, nil
	default:
		return GitHubImportPage{}, errors.New("unexpected stage")
	}
}

func TestNativeProjectRepositoryBindingAndIntake(t *testing.T) {
	t.Parallel()
	backend := &scriptedReconcileBackend{steps: []reconcileStep{{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_repo", Owner: "digitaldrywood", Name: "detent", UpdatedAt: time.Now().UTC()}}}}}
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), ReconcileBackend: backend, ImportBackend: &importFixtureBackend{}})
	f := newNativeFixture(t, service, "", "native-bind")
	existing := f.create(t, "existing-native")
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/integration/repository", testHubAdminToken, map[string]any{"idempotency_key": "bind", "expected_revision": "1", "repository": "digitaldrywood/detent"})
	requireNativeStatus(t, r, http.StatusOK)
	var integration ProjectIntegration
	decodeHubResponse(t, r, &integration)
	if integration.Profile != "native" || integration.Repository != "digitaldrywood/detent" || integration.Intake != "disabled" {
		t.Fatalf("binding = %+v", integration)
	}
	requests := backend.Requests()
	if len(requests) != 1 || !requests[0].SkipIssues || !requests[0].SkipRepository {
		t.Fatalf("binding fetched issue or PR data: %+v", requests)
	}
	r = performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, map[string]any{"idempotency_key": "intake", "expected_revision": fmt.Sprint(integration.Revision), "intake": "manual", "projection": "disabled", "repository_enabled": false})
	requireNativeStatus(t, r, http.StatusOK)
	job := advanceImportFixture(t, f, startImportFixture(t, f, 3, false, 0))
	if !job.IntakePending || job.WorkItemID == string(existing.WorkItemID) {
		t.Fatalf("new native intake = %+v", job)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
	for _, pending := range []bool{true, false} {
		if !pending {
			job = finishImportFixture(t, f, job)
		}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := claimCandidateIDs(t.Context(), tx, claimCandidateQuery{NativeScope: &scope, Scope: string(f.project.ID)}, nil, nil, nil, nil, nil, nil, nil, nil)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		tx.Rollback()
		want := 2
		if pending {
			want = 1
		}
		if len(ids) != want {
			t.Fatalf("claim candidates during intake=%t: %+v", pending, ids)
		}
	}
	if job.IntakePending {
		t.Fatal("completed intake still prevents scheduling")
	}
}

func newIntegrationFixture(t *testing.T, backend ImportBackend) nativeFixture {
	t.Helper()
	s := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), ImportBackend: backend})
	repository, _ := seedProjection(t, s.database.db)
	var project tracker.NativeProject
	if err := s.database.db.QueryRowContext(t.Context(), "SELECT id, organization_id FROM projects WHERE repository_id = ?", repository).Scan(&project.ID, &project.OrganizationID); err != nil {
		t.Fatal(err)
	}
	f := nativeFixture{service: s, project: project, base: "/api/v2/organizations/" + string(project.OrganizationID) + "/projects/" + string(project.ID), token: testHubAdminToken}
	r := performHubAPIRequest(t, s, http.MethodPut, f.base+"/integration", f.token, map[string]any{"idempotency_key": "enable-import", "expected_revision": "1", "intake": "manual", "projection": "disabled", "repository_enabled": false})
	requireNativeStatus(t, r, http.StatusOK)
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

func TestGitHubImportCheckpointCutoverAndNativeIsolation(t *testing.T) {
	t.Parallel()
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("page-failure-%t", partial), func(t *testing.T) {
			backend := &importFixtureBackend{failPage: partial}
			f := newIntegrationFixture(t, backend)
			job := startImportFixture(t, f, 1, false, 0)
			job = advanceImportFixture(t, f, job)
			job = advanceImportFixture(t, f, job)
			stale := job.Revision
			job = advanceImportFixture(t, f, job)
			if partial {
				if job.Status != "partial" || job.Cursor != "page2" || job.Stage != "comments" || job.LastError == "" {
					t.Fatalf("partial checkpoint = %+v", job)
				}
				r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/imports/"+job.ID+"/advance", f.token, map[string]any{"expected_revision": fmt.Sprint(job.Revision)})
				requireNativeStatus(t, r, http.StatusTooManyRequests)
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE github_imports SET retry_after = NULL WHERE id = ?", job.ID); err != nil {
					t.Fatal(err)
				}
			}
			r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/imports/"+job.ID+"/advance", f.token, map[string]any{"expected_revision": fmt.Sprint(stale)})
			requireNativeStatus(t, r, http.StatusConflict)
			job = finishImportFixture(t, f, job)
			if job.Status != "retrieved" || len(job.Gaps) == 0 {
				t.Fatalf("retrieval must retain limitations: %+v", job)
			}
			request := CutoverRequest{Mutation: tracker.Mutation{IdempotencyKey: "preview-unresolved"}, DryRun: true, InitialState: "Todo", States: []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}}}
			receipt := cutoverFixture(t, f, request)
			if receipt.Applied || receipt.UnresolvedDependencies != 1 || len(receipt.Blockers) == 0 {
				t.Fatalf("cutover must retain unresolved dependency: %+v", receipt)
			}
			finishImportFixture(t, f, startImportFixture(t, f, 2, false, 0))
			request.IdempotencyKey = "preview-ready"
			receipt = cutoverFixture(t, f, request)
			if len(receipt.Blockers) != 0 || receipt.IncompleteImports != 0 {
				t.Fatalf("cutover preview = %+v", receipt)
			}
			request.DryRun, request.IdempotencyKey, request.Checkpoint = false, "apply", receipt.Checkpoint
			receipt = cutoverFixture(t, f, request)
			if !receipt.Applied || receipt.Integration.Profile != "native" || receipt.Integration.Intake != "disabled" {
				t.Fatalf("cutover = %+v", receipt)
			}
			cutoverFixture(t, f, request)
			path := f.base + "/work-items/" + job.WorkItemID
			r = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			var issue tracker.NativeIssue
			decodeHubResponse(t, r, &issue)
			if len(issue.Body) < 1000 || len(issue.Dependencies) != 1 {
				t.Fatalf("imported content/dependencies = %+v", issue)
			}
			body := "Native body"
			r = performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "native-edit"}, ExpectedRevision: issue.Revision, Body: &body})
			requireNativeStatus(t, r, http.StatusOK)
			f.create(t, "native creation")
			r = performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "native-comment"}, Body: "Native discussion"})
			requireNativeStatus(t, r, http.StatusOK)
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbox").Scan(&count); err != nil || count != 0 {
				t.Fatalf("native writes queued GitHub mutations: %d, %v", count, err)
			}
			targets, err := f.service.database.reconcileTargets(t.Context())
			if err != nil || len(targets) != 0 {
				t.Fatalf("native idle polling targets = %+v, %v", targets, err)
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
			r = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/imports/"+job.ID+"/records?limit=2", f.token, nil)
			var records tracker.Page[GitHubImportRecord]
			decodeHubResponse(t, r, &records)
			if len(records.Items) != 2 || records.NextCursor == "" {
				t.Fatalf("source export pagination = %+v", records)
			}
		})
	}
}

func TestNativeProfileRejectsGitHubMutationAndRepair(t *testing.T) {
	t.Parallel()
	for _, repositoryEnabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(repositoryEnabled), func(t *testing.T) {
			f := newIntegrationFixture(t, &importFixtureBackend{})
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET profile = 'native', github_repository_enabled = ? WHERE id = ?", repositoryEnabled, f.project.ID); err != nil {
				t.Fatal(err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			now := time.Now().UTC()
			result, err := applyIssueProjection(t.Context(), tx, 1, normalizedIssue{NodeID: "I_issue", Number: 1, Title: "Source overwrite", State: "closed"}, sourceStamp{UpdatedAt: now, Version: "changed"}, now, true)
			if err != nil || result.Changed {
				t.Fatalf("native projection accepted: %+v, %v", result, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			_, err = f.service.ChangeWorkflowState(t.Context(), WorkflowStateChange{IssueID: 1, WorkflowStateID: 1, Mutation: WorkflowLabelMutation{IdempotencyKey: "legacy-write", RepositoryID: 1, IssueID: 1, Label: "detent:done"}})
			if err == nil {
				t.Fatal("native project accepted a legacy workflow label write")
			}
			targets, err := f.service.database.reconcileTargets(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if repositoryEnabled {
				want = 1
			}
			if len(targets) != want {
				t.Fatalf("targets = %+v", targets)
			}
			snapshot := ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_repo", Owner: "digitaldrywood", Name: "detent", UpdatedAt: now}}
			if err := f.service.database.applyReconcileSnapshot(t.Context(), reconcileTarget{RepositoryTarget: RepositoryTarget{ID: 1}}, ReconcileFullRepair, now, now, snapshot); err != nil {
				t.Fatal(err)
			}
			var title, state string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT title, github_state FROM issues WHERE id = 1").Scan(&title, &state); err != nil {
				t.Fatal(err)
			}
			if title != "Issue" || state != "open" {
				t.Fatalf("full repair mutated native data: %q %q", title, state)
			}
		})
	}
}

func TestCutoverSourceClosureAndProjectionCoalescing(t *testing.T) {
	t.Parallel()
	for _, closeSource := range []bool{false, true} {
		t.Run(strconv.FormatBool(closeSource), func(t *testing.T) {
			f := newIntegrationFixture(t, &importFixtureBackend{})
			backend := &recordingOutboxBackend{}
			f.service.config.OutboxBackend = backend
			job := finishImportFixture(t, f, startImportFixture(t, f, 1, false, 0))
			finishImportFixture(t, f, startImportFixture(t, f, 2, false, 0))
			request := CutoverRequest{Mutation: tracker.Mutation{IdempotencyKey: "preview"}, DryRun: true, InitialState: "Todo", States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}, CloseSource: closeSource, DestinationURL: "https://detent.example/projects/native"}
			receipt := cutoverFixture(t, f, request)
			request.IdempotencyKey, request.DryRun, request.Checkpoint = "apply", false, receipt.Checkpoint
			receipt = cutoverFixture(t, f, request)
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbox WHERE status = 'pending'").Scan(&count); err != nil {
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
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbox WHERE status = 'pending'").Scan(&count); err != nil || count != 1 {
				t.Fatalf("coalesced count = %d: %v", count, err)
			}
			backend.failures = []error{errors.New("temporary outage")}
			if _, err := f.service.ProcessOutbox(t.Context()); err == nil {
				t.Fatal("missing projection failure")
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE github_outbox SET next_retry_at = NULL WHERE status = 'retrying'"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.ProcessOutbox(t.Context()); err != nil {
				t.Fatal(err)
			}
			if backend.effects != want+1 {
				t.Fatalf("projection effects = %d", backend.effects)
			}
		})
	}
}
