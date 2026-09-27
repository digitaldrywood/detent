package hubserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// patchLabels replaces the issue's labels and returns what the hub stored.
func patchLabels(t *testing.T, f nativeFixture, issue tracker.NativeIssue, key string, labels []string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token,
		tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedRevision: issue.Revision, Labels: &labels})
	requireNativeStatus(t, response, http.StatusOK)
	var updated tracker.NativeIssue
	decodeHubResponse(t, response, &updated)
	return updated
}

func TestNativeLabelCatalogue(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "labels")

	empty := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/labels", f.token, nil)
	requireNativeStatus(t, empty, http.StatusOK)
	var catalogue nativeLabelList
	decodeHubResponse(t, empty, &catalogue)
	if len(catalogue.Items) != 0 {
		t.Fatalf("a project with no labelled issues has a catalogue of %#v", catalogue.Items)
	}
	// An empty catalogue is an empty array, not null: a client that decodes
	// it into a list must not have to special-case the first label.
	if !strings.Contains(empty.Body.String(), `"items":[]`) {
		t.Fatalf("empty catalogue body = %s", empty.Body.String())
	}

	first := patchLabels(t, f, f.create(t, "one"), "labels-one", []string{"bug", "goal:reliability", "effort:medium"})
	patchLabels(t, f, f.create(t, "two"), "labels-two", []string{"bug", "chore", "priority:high"})
	// A label repeated inside one issue counts once, not twice.
	patchLabels(t, f, first, "labels-one-again", []string{"bug", "bug", "goal:reliability", "effort:medium"})

	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/labels", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &catalogue)

	names := make([]string, 0, len(catalogue.Items))
	counts := map[string]int{}
	for _, label := range catalogue.Items {
		names = append(names, label.Name)
		counts[label.Name] = label.Count
		if !strings.HasPrefix(label.Color, "#") || len(label.Color) != 7 {
			t.Errorf("label %q has colour %q", label.Name, label.Color)
		}
	}
	for _, test := range []struct {
		name  string
		want  bool
		count int
	}{
		{name: "bug", want: true, count: 2},
		{name: "chore", want: true, count: 1},
		{name: "goal:reliability", want: true, count: 1},
		// A managed prefix names a field the hub owns and shows in its own
		// row, so it is never offered as a label to attach.
		{name: "effort:medium"},
		{name: "priority:high"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, present := counts[test.name]
			if present != test.want {
				t.Fatalf("catalogue %v, %q present = %v, want %v", names, test.name, present, test.want)
			}
			if test.want && counts[test.name] != test.count {
				t.Fatalf("%q count = %d, want %d", test.name, counts[test.name], test.count)
			}
		})
	}
	// Busiest first, then alphabetical, so the order is total.
	if len(names) != 3 || names[0] != "bug" || names[1] != "chore" || names[2] != "goal:reliability" {
		t.Fatalf("catalogue order = %v", names)
	}

	// The colour is derived from the name, so two reads cannot disagree.
	second := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/labels", f.token, nil)
	requireNativeStatus(t, second, http.StatusOK)
	if second.Body.String() != response.Body.String() {
		t.Fatalf("two reads disagree:\n%s\n%s", response.Body.String(), second.Body.String())
	}

	// An unsupported query member is refused rather than ignored.
	rejected := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/labels?colour=red", f.token, nil)
	requireNativeStatus(t, rejected, http.StatusUnprocessableEntity)
}

func TestIsManagedLabel(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		label string
		want  bool
	}{
		{name: "plain", label: "bug"},
		{name: "namespaced but unmanaged", label: "goal:reliability"},
		{name: "priority", label: "priority:high", want: true},
		{name: "priority in another case", label: "Priority:High", want: true},
		{name: "effort", label: "effort:low", want: true},
		{name: "reserved detent", label: "detent:coordinator", want: true},
		{name: "blank", label: "   ", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isManagedLabel(test.label); got != test.want {
				t.Fatalf("isManagedLabel(%q) = %v, want %v", test.label, got, test.want)
			}
		})
	}
}

func TestLabelColorIsStableAndInThePalette(t *testing.T) {
	t.Parallel()
	palette := map[string]bool{}
	for _, colour := range labelPalette {
		palette[colour] = true
	}
	for _, test := range []struct{ name, label, same string }{
		{name: "bug", label: "bug", same: "BUG"},
		{name: "chore", label: "chore", same: " chore "},
		{name: "long name", label: "needs-product-decision", same: "Needs-Product-Decision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			colour := labelColor(test.label)
			if !palette[colour] {
				t.Fatalf("labelColor(%q) = %q, which is not in the palette", test.label, colour)
			}
			if other := labelColor(test.same); other != colour {
				t.Fatalf("labelColor(%q) = %q but labelColor(%q) = %q", test.label, colour, test.same, other)
			}
		})
	}
}

// The picker's other two writes: an assignee list, and a priority that is
// removed rather than replaced.
func TestNativeIssueAssigneesAndPriorityClear(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "properties")
	issue := f.create(t, "assignable")
	path := f.base + "/work-items/" + string(issue.WorkItemID)

	// The patches below are raw JSON on purpose: what this endpoint had to
	// learn is an encoding — a member that may be a number, a null or a word
	// — and a Go literal would exercise the helper rather than the bytes a
	// client sends.
	send := func(t *testing.T, key string, revision tracker.Revision, member string) *httptest.ResponseRecorder {
		t.Helper()
		body := fmt.Sprintf(`{"idempotency_key":%q,"expected_revision":"%d"%s}`, key, revision, member)
		request := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+f.token)
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request)
		return response
	}
	patchRaw := func(t *testing.T, key string, revision tracker.Revision, member string) tracker.NativeIssue {
		t.Helper()
		response := send(t, key, revision, member)
		requireNativeStatus(t, response, http.StatusOK)
		var updated tracker.NativeIssue
		decodeHubResponse(t, response, &updated)
		return updated
	}

	assignees := []string{"owner@example.test", "viewer@example.test"}
	response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "assign"}, ExpectedRevision: issue.Revision, Assignees: &assignees,
	})
	requireNativeStatus(t, response, http.StatusOK)
	var assigned tracker.NativeIssue
	decodeHubResponse(t, response, &assigned)
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
			history := performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?limit=100", f.token, nil)
			requireNativeStatus(t, history, http.StatusOK)
			var page tracker.Page[tracker.CollaborationEvent]
			decodeHubResponse(t, history, &page)
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
	requireNativeStatus(t, rejected, http.StatusUnprocessableEntity)
}

func latestRevision(t *testing.T, f nativeFixture, id tracker.NativeWorkItemID) tracker.Revision {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue.Revision
}
