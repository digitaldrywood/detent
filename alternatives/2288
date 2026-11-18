package nativework_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDependenciesAndTransitions(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	a := f.Create(t, "a")
	b := f.Create(t, "b")
	c := f.Create(t, "c")
	for _, test := range []struct {
		name    string
		issue   tracker.NativeIssue
		blocker tracker.NativeWorkItemID
		want    int
	}{
		{"a depends b", a, b.WorkItemID, http.StatusOK},
		{"b depends c", b, c.WorkItemID, http.StatusOK},
		{"cycle", c, a.WorkItemID, http.StatusUnprocessableEntity},
		{"self", c, c.WorkItemID, http.StatusUnprocessableEntity},
		{"missing", c, tracker.NativeWorkItemID(hubapi.NewID("wi")), http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ExpectedRevision: 1, RelatedWorkItemID: test.blocker, Operation: "add"}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(test.issue.WorkItemID)+"/dependencies", f.Token, request), test.want)
		})
	}
	for _, test := range []struct {
		name, operation, relationship string
		target                        tracker.NativeWorkItemID
		changed                       bool
	}{
		{"duplicate dependency", "add", "dependency", b.WorkItemID, false},
		{"absent dependency", "remove", "dependency", c.WorkItemID, false},
		{"new related issue", "add", "related", b.WorkItemID, true},
		{"duplicate related issue", "add", "related", b.WorkItemID, false},
		{"absent related issue", "remove", "related", c.WorkItemID, false},
		{"remove related issue", "remove", "related", b.WorkItemID, true},
		{"removed related issue", "remove", "related", b.WorkItemID, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := hubfixture.ReadWorkItem(t, f, a.WorkItemID, "")
			var beforeVersions, beforeEvents int
			if err := f.Hub.Admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3)", f.Project.OrganizationID, f.Project.ID, a.WorkItemID).Scan(&beforeVersions, &beforeEvents); err != nil {
				t.Fatal(err)
			}
			request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ExpectedRevision: current.Revision, RelatedWorkItemID: test.target, Operation: test.operation, RelationshipType: test.relationship}
			response := f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(a.WorkItemID)+"/dependencies", f.Token, request)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var after tracker.NativeIssue
			hubfixture.Decode(t, response, &after)
			increment := 0
			if test.changed {
				increment = 1
			}
			var versions, events int
			if err := f.Hub.Admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3)", f.Project.OrganizationID, f.Project.ID, a.WorkItemID).Scan(&versions, &events); err != nil {
				t.Fatal(err)
			}
			if after.Revision != current.Revision+tracker.Revision(increment) || versions != beforeVersions+increment || events != beforeEvents+increment {
				t.Fatalf("relationship revision=%d/%d versions=%d/%d events=%d/%d", after.Revision, current.Revision, versions, beforeVersions, events, beforeEvents)
			}
		})
	}
	for _, test := range []struct {
		name, state, reason, detail string
		want                        int
	}{
		{"invalid state", "Unknown", "user_requested", "", http.StatusUnprocessableEntity},
		{"raw reason", "Done", "raw prompt contents", "", http.StatusUnprocessableEntity},
		{"oversized reason detail", "Done", "user_requested", strings.Repeat("x", 8<<10+1), http.StatusUnprocessableEntity},
		{"valid state", "Done", "user_requested", "The operator completed the requested work", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ExpectedRevision: 1, State: test.state, Reason: test.reason, ReasonDetail: test.detail}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(c.WorkItemID)+"/workflow", f.Token, request), test.want)
		})
	}
	response := f.Hub.Request(t, http.MethodGet, f.Base+"/work-items/"+string(c.WorkItemID)+"/history?limit=1", f.Token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var page tracker.Page[tracker.CollaborationEvent]
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("history page = %#v", page)
	}
}

func TestNativeNewestIssuePages(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	for _, name := range []string{"first", "second"} {
		issue := f.Create(t, name)
		path := f.Base + "/work-items/" + string(issue.WorkItemID)
		for index := range 3 {
			response := f.Hub.Request(t, http.MethodPost, path+"/comments", f.Token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("%s-comment-%d", name, index)}, Body: fmt.Sprintf("comment %d", index)})
			hubfixture.RequireStatus(t, response, http.StatusOK)
		}
		for _, list := range []string{"history", "comments"} {
			t.Run(name+"/"+list, func(t *testing.T) {
				params := url.Values{"direction": {"newest"}, "limit": {"1"}}
				if list == "history" {
					params.Set("view", "timeline")
				}
				previous := int64(1<<63 - 1)
				seen := make(map[int64]bool)
				for {
					response := f.Hub.Request(t, http.MethodGet, path+"/"+list+"?"+params.Encode(), f.Token, nil)
					hubfixture.RequireStatus(t, response, http.StatusOK)
					var page struct {
						Items []struct {
							Sequence          int64 `json:"sequence,string"`
							AggregateSequence int64 `json:"aggregate_sequence,string"`
						} `json:"items"`
						NextCursor string `json:"next_cursor"`
					}
					hubfixture.Decode(t, response, &page)
					if len(page.Items) != 1 {
						t.Fatalf("page length = %d", len(page.Items))
					}
					sequence := page.Items[0].Sequence
					if list == "history" {
						sequence = page.Items[0].AggregateSequence
					}
					if sequence >= previous || seen[sequence] {
						t.Fatalf("non-descending sequence %d after %d", sequence, previous)
					}
					seen[sequence], previous = true, sequence
					if page.NextCursor == "" {
						break
					}
					params.Set("cursor", page.NextCursor)
					wrong := params.Encode()
					wrong = strings.Replace(wrong, "direction=newest", "direction=oldest", 1)
					hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, path+"/"+list+"?"+wrong, f.Token, nil), http.StatusUnprocessableEntity)
				}
				want := 3
				if list == "history" {
					want++
				}
				if len(seen) != want {
					t.Fatalf("received %d records, want %d", len(seen), want)
				}
			})
		}
	}
}

func TestNativeCommentsProvenanceAndIdempotency(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	issue := f.Create(t, "discussion")
	path := f.Base + "/work-items/" + string(issue.WorkItemID) + "/comments"
	sourceTime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	request := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "Explicit user comment may include sensitive text: example-secret-value", Provenance: &tracker.Provenance{Provider: "github", ExternalID: "source-comment", AuthorID: "external-author", CreatedAt: sourceTime, UpdatedAt: sourceTime, ObservedAt: sourceTime.Add(time.Hour)}}
	response := f.Hub.Request(t, http.MethodPost, path, f.Token, request)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var comment tracker.NativeComment
	hubfixture.Decode(t, response, &comment)
	if comment.Actor.PrincipalID == "external-author" || comment.Provenance.AuthorID != "external-author" || !comment.CreatedAt.After(sourceTime) {
		t.Fatalf("comment provenance = %#v", comment)
	}
	for range 4 {
		retry := f.Hub.Request(t, http.MethodPost, path, f.Token, request)
		hubfixture.RequireStatus(t, retry, http.StatusOK)
		if retry.Body.String() != response.Body.String() {
			t.Fatal("retry changed committed response")
		}
	}
	request.Body = "Changed payload"
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, f.Token, request), http.StatusConflict)
	edit := tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-comment"}, ExpectedRevision: 1, Body: "Edited content"}
	response = f.Hub.Request(t, http.MethodPatch, path+"/"+comment.ID, f.Token, edit)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, &comment)
	if comment.Revision != 2 || comment.EditedBy == nil || comment.Provenance.AuthorID != "external-author" {
		t.Fatalf("edited comment = %#v", comment)
	}
	request.IdempotencyKey = "repeat-import"
	response = f.Hub.Request(t, http.MethodPost, path, f.Token, request)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var importedAgain tracker.NativeComment
	hubfixture.Decode(t, response, &importedAgain)
	if importedAgain.ID != comment.ID || importedAgain.Body != "Edited content" {
		t.Fatalf("reimport overwrote native edit: %#v", importedAgain)
	}
	response = f.Hub.Request(t, http.MethodGet, path+"/"+comment.ID+"/versions/1", f.Token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var original tracker.NativeComment
	hubfixture.Decode(t, response, &original)
	if !strings.Contains(original.Body, "example-secret-value") || original.Revision != 1 {
		t.Fatalf("original revision = %#v", original)
	}
	for index := range 3 {
		f.Create(t, fmt.Sprintf("another-%d", index))
	}
	response = f.Hub.Request(t, http.MethodGet, path, f.Token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var comments tracker.Page[tracker.NativeComment]
	hubfixture.Decode(t, response, &comments)
	if len(comments.Items) != 1 || comments.Items[0].Body != "Edited content" {
		t.Fatalf("comments = %#v", comments)
	}
}
