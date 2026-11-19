package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeNoOpRevisions(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "no-op edits")
	scope := nativeBoardFixtureScope(t, f)
	admin := conversationTestAdmin(t, f.service)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	states := nativeFixtureStates()
	states[0].Transitions = append(states[0].Transitions, "Todo")
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), scope.Project)
		if err != nil {
			return err
		}
		project.States = states
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		reason string
		status int
	}{
		{reason: "worker_progress", status: http.StatusUnprocessableEntity},
		{reason: "dependency_ready", status: http.StatusUnprocessableEntity},
		{reason: "user_requested", status: http.StatusOK},
	} {
		t.Run("same state command "+test.reason, func(t *testing.T) {
			var beforeVersions, beforeEvents int
			if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&beforeVersions, &beforeEvents); err != nil {
				t.Fatal(err)
			}
			for index := range 2 {
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("same-state-%s-%d", test.reason, index)}, ExpectedRevision: issue.Revision, State: issue.State, Reason: test.reason})
				requireNativeStatus(t, response, test.status)
				var result tracker.NativeIssue
				if test.status == http.StatusOK {
					decodeHubResponse(t, response, &result)
				} else {
					result = readWorkItem(t, f, issue.WorkItemID, "")
				}
				if result.Revision != issue.Revision || !result.UpdatedAt.Equal(issue.UpdatedAt) {
					t.Fatalf("same-state command changed revision or timestamp: %+v", result)
				}
			}
			var versions, events int
			if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&versions, &events); err != nil {
				t.Fatal(err)
			}
			if versions != beforeVersions || events != beforeEvents {
				t.Fatalf("same-state command wrote history: versions=%d/%d events=%d/%d", versions, beforeVersions, events, beforeEvents)
			}
		})
	}
	var beforeVersions, beforeEvents int
	if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&beforeVersions, &beforeEvents); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"title", "body", "labels", "assignees", "priority"} {
		request := map[string]any{"expected_revision": fmt.Sprint(issue.Revision), "idempotency_key": "identical-" + field, field: map[string]any{"title": issue.Title, "body": issue.Body, "labels": issue.Labels, "assignees": issue.Assignees, "priority": issue.Priority}[field]}
		before, beforeVersion, err := f.service.readWorkItemRecord(t.Context(), scope, string(issue.WorkItemID))
		if err != nil {
			t.Fatal(err)
		}
		var updates int
		ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, statement string) {
			if strings.Contains(strings.ToUpper(statement), "UPDATE ISSUES") {
				updates++
			}
		})
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(raw)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.token)
		response := httptest.NewRecorder()
		f.service.echo.ServeHTTP(response, req)
		requireNativeStatus(t, response, http.StatusOK)
		var result tracker.NativeIssue
		decodeHubResponse(t, response, &result)
		after, afterVersion, err := f.service.readWorkItemRecord(t.Context(), scope, string(issue.WorkItemID))
		if err != nil {
			t.Fatal(err)
		}
		var versions, events int
		if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&versions, &events); err != nil {
			t.Fatal(err)
		}
		if result.Revision != issue.Revision || !result.UpdatedAt.Equal(issue.UpdatedAt) || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeVersion, afterVersion) || versions != beforeVersions || events != beforeEvents || updates != 0 {
			t.Fatalf("identical %s changed record or history: versions %d/%d events %d/%d updates %d", field, versions, beforeVersions, events, beforeEvents, updates)
		}
	}
	changed := "real title change"
	var realUpdates int
	ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, statement string) {
		if strings.Contains(strings.ToUpper(statement), "UPDATE ISSUES") {
			realUpdates++
		}
	})
	raw, err := json.Marshal(tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "real-edit"}, ExpectedRevision: issue.Revision, Title: &changed})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	response := httptest.NewRecorder()
	f.service.echo.ServeHTTP(response, req)
	requireNativeStatus(t, response, http.StatusOK)
	var result tracker.NativeIssue
	decodeHubResponse(t, response, &result)
	if result.Revision != issue.Revision+1 || realUpdates == 0 {
		t.Fatalf("real edit revision=%d want=%d issue updates=%d", result.Revision, issue.Revision+1, realUpdates)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "stale-identical"}, ExpectedRevision: issue.Revision, Title: &changed}), http.StatusConflict)
	response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "no-op-comment"}, Body: "comment content"})
	requireNativeStatus(t, response, http.StatusOK)
	var comment tracker.NativeComment
	decodeHubResponse(t, response, &comment)
	for index, body := range []string{comment.Body, "changed comment", "changed comment"} {
		response = performHubAPIRequest(t, f.service, http.MethodPatch, path+"/comments/"+comment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("comment-edit-%d", index)}, ExpectedRevision: comment.Revision, Body: body})
		requireNativeStatus(t, response, http.StatusOK)
		var edited tracker.NativeComment
		decodeHubResponse(t, response, &edited)
		want := comment.Revision
		if body != comment.Body {
			want++
		}
		if edited.Revision != want {
			t.Fatalf("comment revision=%d want %d", edited.Revision, want)
		}
		comment = edited
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path+"/comments/"+comment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "stale-comment"}, ExpectedRevision: 1, Body: comment.Body}), http.StatusConflict)
	var versions, events int
	if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&versions, &events); err != nil {
		t.Fatal(err)
	}
	if versions != beforeVersions+3 || events != beforeEvents+3 {
		t.Fatalf("expected one real issue edit, comment creation and comment edit: versions=%d/%d events=%d/%d", versions, beforeVersions, events, beforeEvents)
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "real-transition"}, ExpectedRevision: result.Revision, State: "In Progress", Reason: "user_requested"})
	requireNativeStatus(t, response, http.StatusOK)
	var transitioned tracker.NativeIssue
	decodeHubResponse(t, response, &transitioned)
	if transitioned.Revision != result.Revision+1 || transitioned.State != "In Progress" {
		t.Fatalf("real transition did not advance once: %+v", transitioned)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "stale-transition"}, ExpectedRevision: result.Revision, State: "In Progress", Reason: "user_requested"}), http.StatusConflict)
	if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&versions, &events); err != nil {
		t.Fatal(err)
	}
	if versions != beforeVersions+4 || events != beforeEvents+4 {
		t.Fatalf("real transition wrote extra history: versions=%d/%d events=%d/%d", versions, beforeVersions, events, beforeEvents)
	}
	t.Run("operator hold response and replay", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
		states[1].Transitions = append(states[1].Transitions, "Blocked")
		states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Blocked", "Todo"}})
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(t.Context(), scope.Project)
			if err != nil {
				return err
			}
			project.States = states
			_, err = w.UpdateProjectRecord(t.Context(), project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		clock := f.service.config.Clock
		f.service.config.Clock = func() time.Time { return now }
		t.Cleanup(func() { f.service.config.Clock = clock })
		request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "enter-blocked"}, ExpectedRevision: transitioned.Revision, State: "Blocked", Reason: "user_requested"}
		response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, request)
		requireNativeStatus(t, response, http.StatusOK)
		var blocked tracker.NativeIssue
		decodeHubResponse(t, response, &blocked)
		now = now.Add(time.Minute)
		request.IdempotencyKey = "retain-operator-hold"
		request.ExpectedRevision = blocked.Revision
		for _, operation := range []string{"hold", "replay"} {
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var result tracker.NativeIssue
			decodeHubResponse(t, response, &result)
			current := readWorkItem(t, f, issue.WorkItemID, "")
			if result.Revision != blocked.Revision || !result.UpdatedAt.Equal(blocked.UpdatedAt) || !result.LastActivityAt.Equal(now) || !result.LastActivityAt.Equal(current.LastActivityAt) {
				t.Fatalf("%s returned stale operator hold: revision=%d/%d activity=%s/%s", operation, result.Revision, blocked.Revision, result.LastActivityAt, current.LastActivityAt)
			}
		}
		var holdVersions, holdEvents int
		if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&holdVersions, &holdEvents); err != nil {
			t.Fatal(err)
		}
		if holdVersions != versions+1 || holdEvents != events+2 {
			t.Fatalf("operator hold or replay wrote extra history: versions=%d/%d events=%d/%d", holdVersions, versions, holdEvents, events)
		}
	})
}
