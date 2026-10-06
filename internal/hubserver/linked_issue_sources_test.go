package hubserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func linkedFixture(t *testing.T) nativeFixture {
	t.Helper()
	f := newNativeFixture(t, nil, "", "linked")
	f.service.config.ImportBackend = linkedTestImporter{snapshot: linkedSnapshot()}
	now := formatHubTime(time.Now())
	_, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO repositories (github_node_id, github_owner, github_name, created_at, updated_at) VALUES ('R_repo', 'acme', 'orders', ?, ?)", now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.database.db.ExecContext(t.Context(), "DELETE FROM projects WHERE repository_id IS NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET repository_id = (SELECT id FROM repositories WHERE github_node_id = 'R_repo') WHERE id = ?", f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	states := append(nativeFixtureStates(), tracker.NativeState{Name: "Cancelled", Terminal: true})
	if err := applyNativeProjectStates(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, states, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f nativeFixture) link(t *testing.T, key string) tracker.NativeIssue {
	t.Helper()
	r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, GitHubIssueURL: "github.com/Acme/Orders/issues/12", State: "Todo"})
	requireNativeStatus(t, r, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, r, &issue)
	return issue
}

func TestLinkedIssueCreation(t *testing.T) {
	t.Parallel()
	f := linkedFixture(t)
	first := f.link(t, "link")
	if len(first.ExternalReferences) < 1 || first.ExternalReferences[0].Repository != "acme/orders" || first.ExternalReferences[0].Number != 12 {
		t.Fatalf("linked issue lost source reference: %+v", first.ExternalReferences)
	}
	if first.LinkedSource == nil || first.LinkedSource.Status != "complete" || first.LinkedSource.URL != "https://github.com/acme/orders/issues/12" {
		t.Fatalf("linked issue = %#v", first)
	}
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Go(func() {
			if got := f.link(t, fmt.Sprint("duplicate-", i)); got.WorkItemID != first.WorkItemID {
				t.Errorf("duplicate issue = %s", got.WorkItemID)
			}
		})
	}
	wg.Wait()
	for _, url := range []string{"https://github.com/acme/unrelated/issues/12", "https://github.com/acme/orders/pull/12", "https://example.com/acme/orders/issues/12"} {
		r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: url}, GitHubIssueURL: url, State: "Todo"})
		requireNativeStatus(t, r, http.StatusUnprocessableEntity)
	}
}

func linkedSnapshot() tracker.GitHubIssueSnapshot {
	now := time.Now().UTC()
	prov := tracker.Provenance{Provider: "github", ExternalID: "I_source", AuthorID: "author", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute), ObservedAt: now}
	comment := prov
	comment.ExternalID = "C_source"
	return tracker.GitHubIssueSnapshot{URL: "https://github.com/acme/orders/issues/12", Title: "Fetched source title", Body: "Full source body", Provenance: prov, Comments: []tracker.GitHubIssueComment{{Body: "Source discussion", Provenance: comment}}}
}

func TestLinkedIssueIntakeAtomicAndNativeEdits(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"unedited", "native edits", "edit back to placeholder"} {
		t.Run(mode, func(t *testing.T) {
			f := linkedFixture(t)
			issue := pendingLinkedIssue(t, f)
			path := f.base + "/work-items/" + string(issue.WorkItemID)
			title, body := issue.Title, issue.Body
			if mode != "unedited" {
				if mode == "native edits" {
					title, body = "Native title", "Native body"
				}
				r := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit"}, ExpectedRevision: issue.Revision, Title: &title, Body: &body})
				requireNativeStatus(t, r, http.StatusOK)
			}
			worker := f.worker(t, "worker")
			descriptor := hubTestPolicy()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "runner", "hostname": "runner", "version": "test", "capacity": 1}), http.StatusOK)
			r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{WorkItemID: issue.WorkItemID, MachineID: "runner", SessionID: "session", PolicyID: descriptor.ID, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
			requireNativeStatus(t, r, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, r, &lease)
			snapshot := linkedSnapshot()
			request := tracker.GitHubIntake{Mutation: tracker.Mutation{IdempotencyKey: "intake", LeaseID: lease.ID, FencingToken: lease.FencingToken}, Snapshot: snapshot}
			invalid := request
			invalid.IdempotencyKey = "invalid"
			invalid.Snapshot.Comments = append([]tracker.GitHubIssueComment{}, snapshot.Comments...)
			invalid.Snapshot.Comments = append(invalid.Snapshot.Comments, tracker.GitHubIssueComment{})
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/source-intake", worker, invalid), http.StatusUnprocessableEntity)
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id = ?", issue.WorkItemID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial comments = %d, error = %v", count, err)
			}
			stale := request
			stale.IdempotencyKey, stale.FencingToken = "stale", lease.FencingToken+1
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/source-intake", worker, stale), http.StatusConflict)
			var wg sync.WaitGroup
			responses := make(chan *httptest.ResponseRecorder, 3)
			for i := range 3 {
				wg.Go(func() {
					next := request
					next.IdempotencyKey = fmt.Sprint("retry-", i)
					responses <- performHubAPIRequest(t, f.service, http.MethodPost, path+"/source-intake", worker, next)
				})
			}
			wg.Wait()
			close(responses)
			for r := range responses {
				requireNativeStatus(t, r, http.StatusOK)
			}
			r = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, r, http.StatusOK)
			var hydrated tracker.NativeIssue
			decodeHubResponse(t, r, &hydrated)
			if mode == "unedited" {
				title, body = snapshot.Title, snapshot.Body
			}
			if hydrated.Title != title || hydrated.Body != body || hydrated.LinkedSource.Status != "complete" || hydrated.LinkedSource.Snapshot.Body != snapshot.Body || hydrated.Provenance.ExternalID != "I_source" {
				t.Fatalf("hydrated = %#v", hydrated)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id = ?", issue.WorkItemID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("comments = %d, error = %v", count, err)
			}
			request.IdempotencyKey = "later-source-edit"
			request.Snapshot.Title, request.Snapshot.Body = "Updated GitHub title", "Updated GitHub body"
			request.Snapshot.Provenance.UpdatedAt = snapshot.Provenance.UpdatedAt.Add(time.Second)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/source-intake", worker, request), http.StatusOK)
			r = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &hydrated)
			if mode == "unedited" {
				title, body = request.Snapshot.Title, request.Snapshot.Body
			}
			if hydrated.Title != title || hydrated.Body != body || hydrated.LinkedSource.Snapshot.Body != request.Snapshot.Body {
				t.Fatalf("later GitHub edit = %#v", hydrated)
			}
		})
	}
}
