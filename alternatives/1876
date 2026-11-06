package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type importFixtureBackend struct {
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
		return GitHubImportPage{Issue: &IssueSource{NodeID: id, Number: request.IssueNumber, Title: "Imported issue", Body: body, URL: fmt.Sprintf("https://github.com/digitaldrywood/detent/issues/%d", request.IssueNumber), State: "open", AuthorID: "author", Labels: []string{"Todo"}, Assignees: []string{}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}, Records: []GitHubImportRecord{{SourceKey: "issue:" + id, Kind: "issue", Data: json.RawMessage(`{"id":"` + id + `"}`)}}, Gaps: []string{"Deleted comments and edit revisions are unavailable"}}, nil
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
	s := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), ImportBackend: backend})
	repository, _ := seedProjection(t, s.database.db)
	var project tracker.NativeProject
	if err := s.database.db.QueryRowContext(t.Context(), "SELECT id, organization_id FROM projects WHERE repository_id = ?", repository).Scan(&project.ID, &project.OrganizationID); err != nil {
		t.Fatal(err)
	}
	f := nativeFixture{service: s, project: project, base: "/api/v2/organizations/" + string(project.OrganizationID) + "/projects/" + string(project.ID), token: testHubAdminToken}
	seedNativeFixturePolicy(t, s, string(project.OrganizationID)+"/"+string(project.ID))
	if _, err := s.database.db.ExecContext(t.Context(), "UPDATE projects SET duplicate_challenge=0 WHERE id=?", project.ID); err != nil {
		t.Fatal(err)
	}
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
