package nativework_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// patchLabels replaces the issue's labels and returns what the hub stored.
func patchLabels(t *testing.T, f hubfixture.Native, issue tracker.NativeIssue, key string, labels []string) tracker.NativeIssue {
	t.Helper()
	response := f.Hub.Request(t, http.MethodPatch, f.Base+"/work-items/"+string(issue.WorkItemID), f.Token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedRevision: issue.Revision, Labels: &labels})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var updated tracker.NativeIssue
	hubfixture.Decode(t, response, &updated)
	return updated
}

// The picker's other two writes: an assignee list, and a priority that is
// removed rather than replaced.
func TestNativeIssueAssigneesAndPriorityClear(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubserver.Config{})
	issue := f.Create(t, "assignable")
	path := f.Base + "/work-items/" + string(issue.WorkItemID)

	// The patches below are raw JSON on purpose: what this endpoint had to
	// learn is an encoding — a member that may be a number, a null or a word
	// — and a Go literal would exercise the helper rather than the bytes a
	// client sends.
	send := func(t *testing.T, key string, revision tracker.Revision, member string) *httptest.ResponseRecorder {
		t.Helper()
		body := fmt.Sprintf(`{"idempotency_key":%q,"expected_revision":"%d"%s}`, key, revision, member)
		request := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+f.Token)
		response := httptest.NewRecorder()
		f.Hub.Handler().ServeHTTP(response, request)
		return response
	}
	patchRaw := func(t *testing.T, key string, revision tracker.Revision, member string) tracker.NativeIssue {
		t.Helper()
		response := send(t, key, revision, member)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var updated tracker.NativeIssue
		hubfixture.Decode(t, response, &updated)
		return updated
	}

	assignees := []string{"owner@example.test", "viewer@example.test"}
	response := f.Hub.Request(t, http.MethodPatch, path, f.Token, tracker.UpdateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "assign"}, ExpectedRevision: issue.Revision, Assignees: &assignees,
	})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var assigned tracker.NativeIssue
	hubfixture.Decode(t, response, &assigned)
	if len(assigned.Assignees) != 2 || assigned.Assignees[0] != "owner@example.test" {
		t.Fatalf("assignees = %v", assigned.Assignees)
	}

	urgent := patchRaw(t, "urgent", assigned.Revision, `,"priority":0`)
	if urgent.Priority == nil || *urgent.Priority != 0 {
		t.Fatalf("priority = %v, want 0", urgent.Priority)
	}
	// An edit that says nothing about the priority leaves it alone. That is
	// the property the clear had to be added without breaking.
	retitled := patchRaw(t, "retitle", urgent.Revision, `,"title":"Still urgent"`)
	if retitled.Priority == nil || *retitled.Priority != 0 {
		t.Fatalf("priority after an unrelated edit = %v, want 0", retitled.Priority)
	}

	for _, test := range []struct {
		name   string
		key    string
		member string
	}{
		{name: "the word none", key: "clear-word", member: `,"priority":"none"`},
		{name: "a json null", key: "clear-null", member: `,"priority":null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Put a priority back first, so each case clears a real one.
			current := patchRaw(t, "set-"+test.key, latestRevision(t, f, issue.WorkItemID), `,"priority":1`)
			cleared := patchRaw(t, test.key, current.Revision, test.member)
			if cleared.Priority != nil {
				t.Fatalf("priority after a clear = %d, want none", *cleared.Priority)
			}
			// Clearing a priority is a fact about the issue, so it lands in
			// the history the activity feed reads.
			history := f.Hub.Request(t, http.MethodGet, path+"/history?limit=100", f.Token, nil)
			hubfixture.RequireStatus(t, history, http.StatusOK)
			var page tracker.Page[tracker.CollaborationEvent]
			hubfixture.Decode(t, history, &page)
			recorded := false
			for _, event := range page.Items {
				if event.Type == "issue.edited" && event.Data.Revision == cleared.Revision && slices.Contains(event.Data.Fields, "priority") {
					recorded = true
				}
			}
			if !recorded {
				t.Fatalf("no issue.edited event naming priority at revision %d", cleared.Revision)
			}
		})
	}

	// A priority that is neither a number, null nor "none" is refused rather
	// than silently ignored.
	rejected := send(t, "bad-priority", latestRevision(t, f, issue.WorkItemID), `,"priority":"urgent"`)
	hubfixture.RequireStatus(t, rejected, http.StatusUnprocessableEntity)
}

func latestRevision(t *testing.T, f hubfixture.Native, id tracker.NativeWorkItemID) tracker.Revision {
	t.Helper()
	response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items/"+string(id), f.Token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	return issue.Revision
}
