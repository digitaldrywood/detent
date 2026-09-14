package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// fakeCoordinatorReader serves the bounded hub reads a coordinator turn
// makes. Every page it returns is already in the hub's wire shape.
type fakeCoordinatorReader struct {
	mu          sync.Mutex
	issues      []tracker.NativeIssue
	attempts    map[tracker.NativeWorkItemID][]tracker.NativeAttempt
	comments    map[tracker.NativeWorkItemID][]tracker.NativeComment
	pageSize    int
	issuesErr   error
	issueErr    error
	attemptsErr error
	commentsErr error
	queries     []url.Values
	attemptRead int
	emptyPages  bool
}

func (f *fakeCoordinatorReader) Issue(_ context.Context, id tracker.NativeWorkItemID) (tracker.NativeIssue, error) {
	if f.issueErr != nil {
		return tracker.NativeIssue{}, f.issueErr
	}
	for _, issue := range f.issues {
		if issue.WorkItemID == id {
			return issue, nil
		}
	}
	return tracker.NativeIssue{}, errors.New("work item not found")
}

func (f *fakeCoordinatorReader) Issues(_ context.Context, query url.Values) (tracker.Page[tracker.NativeIssue], error) {
	if f.issuesErr != nil {
		return tracker.Page[tracker.NativeIssue]{}, f.issuesErr
	}
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	if f.emptyPages {
		// A hub that keeps offering a cursor it never fills.
		return tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}, NextCursor: "next"}, nil
	}
	offset := 0
	if cursor := query.Get("cursor"); cursor != "" {
		offset, _ = strconv.Atoi(cursor)
	}
	size := f.pageSize
	if size <= 0 {
		size = len(f.issues)
	}
	end := min(offset+size, len(f.issues))
	page := tracker.Page[tracker.NativeIssue]{Items: f.issues[offset:end]}
	if end < len(f.issues) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}

func (f *fakeCoordinatorReader) Comments(_ context.Context, id tracker.NativeWorkItemID, _ string) (tracker.Page[tracker.NativeComment], error) {
	if f.commentsErr != nil {
		return tracker.Page[tracker.NativeComment]{}, f.commentsErr
	}
	return tracker.Page[tracker.NativeComment]{Items: f.comments[id]}, nil
}

func (f *fakeCoordinatorReader) Attempts(_ context.Context, id tracker.NativeWorkItemID, _ string) (tracker.Page[tracker.NativeAttempt], error) {
	f.mu.Lock()
	f.attemptRead++
	f.mu.Unlock()
	if f.attemptsErr != nil {
		return tracker.Page[tracker.NativeAttempt]{}, f.attemptsErr
	}
	return tracker.Page[tracker.NativeAttempt]{Items: f.attempts[id]}, nil
}

func (f *fakeCoordinatorReader) attemptReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attemptRead
}

func (f *fakeCoordinatorReader) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func coordinatorTestIssue(id, title, state string, terminal bool) tracker.NativeIssue {
	return tracker.NativeIssue{
		NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(id), ProjectID: "prj_1", Number: 1},
		Title:           title,
		State:           state,
		Terminal:        terminal,
		UpdatedAt:       time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
	}
}

func decodeCoordinatorResult(t *testing.T, result AgentToolResult) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result.Content), &decoded); err != nil {
		t.Fatalf("decode tool result %q: %v", result.Content, err)
	}
	return decoded
}

func callCoordinatorTool(t *testing.T, toolset *CoordinatorToolset, name, arguments string) AgentToolResult {
	t.Helper()
	result, err := toolset.Handle(t.Context(), AgentToolCall{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatalf("%s returned a turn error: %v", name, err)
	}
	return result
}

func TestCoordinatorToolsetTools(t *testing.T) {
	t.Parallel()
	toolset := NewCoordinatorToolset(&fakeCoordinatorReader{}, "prj_1", nil, nil)
	names := make([]string, 0, 3)
	for _, tool := range toolset.Tools() {
		names = append(names, tool.Name)
		if !json.Valid(tool.InputSchema) || strings.TrimSpace(tool.Description) == "" {
			t.Fatalf("tool %q has an invalid schema or empty description", tool.Name)
		}
	}
	want := []string{coordinatorToolListAttention, coordinatorToolExplainIssue, coordinatorToolProposeIssue}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

func TestCoordinatorListAttentionBuckets(t *testing.T) {
	t.Parallel()
	reader := &fakeCoordinatorReader{
		issues: []tracker.NativeIssue{
			coordinatorTestIssue("wi_run", "Running work", "In Progress", false),
			coordinatorTestIssue("wi_block", "Blocked state", "Blocked", false),
			coordinatorTestIssue("wi_dep", "Blocked by dependency", "Todo", false),
			coordinatorTestIssue("wi_review", "In review", "In Review", false),
			coordinatorTestIssue("wi_idle", "Nothing to report", "Todo", false),
			coordinatorTestIssue("wi_done", "Finished", "Done", true),
		},
		attempts: map[tracker.NativeWorkItemID][]tracker.NativeAttempt{
			"wi_run": {
				{Status: "completed", StartedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
				{Status: "running", StartedAt: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)},
			},
			"wi_review": {{Status: "completed", StartedAt: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)}},
		},
	}
	reader.issues[2].Blockers = []tracker.NativeDependency{{ID: "wi_block", Terminal: false}}
	reader.issues[4].Blockers = []tracker.NativeDependency{{ID: "wi_done", Terminal: true}}
	toolset := NewCoordinatorToolset(reader, "prj_1", nil, nil)

	result := callCoordinatorTool(t, toolset, coordinatorToolListAttention, `{}`)
	if !result.Success {
		t.Fatalf("list_attention failed: %s", result.Content)
	}
	decoded := decodeCoordinatorResult(t, result)
	for _, test := range []struct {
		bucket string
		want   []string
	}{
		{"running", []string{"wi_run"}},
		{"blocked", []string{"wi_block", "wi_dep"}},
		{"review", []string{"wi_review"}},
	} {
		items, _ := decoded[test.bucket].([]any)
		got := make([]string, 0, len(items))
		for _, item := range items {
			entry, _ := item.(map[string]any)
			id, _ := entry["work_item_id"].(string)
			got = append(got, id)
			if reason, _ := entry["reason"].(string); strings.TrimSpace(reason) == "" {
				t.Fatalf("%s item %s has no reason", test.bucket, id)
			}
		}
		if strings.Join(got, ",") != strings.Join(test.want, ",") {
			t.Fatalf("%s = %v, want %v", test.bucket, got, test.want)
		}
	}
	if projectID, _ := decoded["project_id"].(string); projectID != "prj_1" {
		t.Fatalf("project_id = %v, want prj_1", decoded["project_id"])
	}
}

func TestCoordinatorListAttentionBounds(t *testing.T) {
	t.Parallel()
	reader := &fakeCoordinatorReader{pageSize: 25}
	for i := range 260 {
		issue := coordinatorTestIssue("wi_"+strconv.Itoa(i), strings.Repeat("t", 900), "Blocked", false)
		reader.issues = append(reader.issues, issue)
	}
	toolset := NewCoordinatorToolset(reader, "prj_1", nil, nil)
	result := callCoordinatorTool(t, toolset, coordinatorToolListAttention, `{"limit":50}`)
	if !result.Success {
		t.Fatalf("list_attention failed: %s", result.Content)
	}
	if len(result.Content) > coordinatorToolResultBytes {
		t.Fatalf("result is %d bytes, want at most %d", len(result.Content), coordinatorToolResultBytes)
	}
	decoded := decodeCoordinatorResult(t, result)
	blocked, _ := decoded["blocked"].([]any)
	if len(blocked) != coordinatorAttentionMax {
		t.Fatalf("blocked = %d items, want %d", len(blocked), coordinatorAttentionMax)
	}
	if truncated, _ := decoded["truncated"].(bool); !truncated {
		t.Fatal("truncated = false, want true once the scan bound is reached")
	}
	// Four pages of 25 cover the 100-issue scan bound; the reader is never
	// asked for more.
	if got := reader.queryCount(); got != coordinatorAttentionScan/25 {
		t.Fatalf("issue pages fetched = %d, want %d", got, coordinatorAttentionScan/25)
	}
	first, _ := blocked[0].(map[string]any)
	if title, _ := first["title"].(string); len([]rune(title)) > coordinatorSummaryRunes+1 {
		t.Fatalf("title is %d runes, want it bounded to %d", len([]rune(title)), coordinatorSummaryRunes)
	}
}

func TestCoordinatorExplainIssue(t *testing.T) {
	t.Parallel()
	issue := coordinatorTestIssue("wi_1", "Explain me", "In Review", false)
	issue.Body = strings.Repeat("b", coordinatorIssueBodyRunes+500)
	reader := &fakeCoordinatorReader{issues: []tracker.NativeIssue{issue}}
	reader.attempts = map[tracker.NativeWorkItemID][]tracker.NativeAttempt{"wi_1": {}}
	for i := range 8 {
		reader.attempts["wi_1"] = append(reader.attempts["wi_1"], tracker.NativeAttempt{
			Status: "completed", StartedAt: time.Date(2026, 9, i+1, 0, 0, 0, 0, time.UTC),
		})
	}
	reader.comments = map[tracker.NativeWorkItemID][]tracker.NativeComment{"wi_1": {}}
	for i := range 9 {
		reader.comments["wi_1"] = append(reader.comments["wi_1"], tracker.NativeComment{
			ID: "cmt_" + strconv.Itoa(i), Body: strings.Repeat("c", coordinatorCommentRunes+100), Sequence: int64(i),
			Actor: tracker.Actor{Kind: "human", PrincipalID: "p1"},
		})
	}
	toolset := NewCoordinatorToolset(reader, "prj_1", nil, nil)

	result := callCoordinatorTool(t, toolset, coordinatorToolExplainIssue, `{"work_item_id":"wi_1"}`)
	if !result.Success {
		t.Fatalf("explain_issue failed: %s", result.Content)
	}
	decoded := decodeCoordinatorResult(t, result)
	if len([]rune(decoded["body"].(string))) > coordinatorIssueBodyRunes+1 {
		t.Fatalf("body is not bounded to %d runes", coordinatorIssueBodyRunes)
	}
	attempts, _ := decoded["attempts"].([]any)
	if len(attempts) != coordinatorAttemptCount {
		t.Fatalf("attempts = %d, want the latest %d", len(attempts), coordinatorAttemptCount)
	}
	comments, _ := decoded["comments"].([]any)
	if len(comments) != coordinatorCommentCount {
		t.Fatalf("comments = %d, want the last %d", len(comments), coordinatorCommentCount)
	}
	last, _ := comments[len(comments)-1].(map[string]any)
	if id, _ := last["comment_id"].(string); id != "cmt_8" {
		t.Fatalf("last comment = %v, want cmt_8", last["comment_id"])
	}
	if len([]rune(last["body"].(string))) > coordinatorCommentRunes+1 {
		t.Fatalf("comment body is not bounded to %d runes", coordinatorCommentRunes)
	}
}

func TestCoordinatorProposeIssuePostsStatus(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		posted  []map[string]any
		summary string
	)
	post := func(_ context.Context, data map[string]any, text string) error {
		mu.Lock()
		defer mu.Unlock()
		posted = append(posted, data)
		summary = text
		return nil
	}
	toolset := NewCoordinatorToolset(&fakeCoordinatorReader{}, "prj_1", post, nil)
	result := callCoordinatorTool(t, toolset, coordinatorToolProposeIssue, `{"title":"Add a retry","objective":"Retry the flaky push once."}`)
	if !result.Success {
		t.Fatalf("propose_issue failed: %s", result.Content)
	}
	decoded := decodeCoordinatorResult(t, result)
	proposal, _ := decoded["proposal"].(map[string]any)
	if proposal["title"] != "Add a retry" || proposal["project_id"] != "prj_1" || proposal["objective"] != "Retry the flaky push once." {
		t.Fatalf("proposal = %#v", proposal)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 {
		t.Fatalf("status posts = %d, want 1", len(posted))
	}
	if _, ok := posted[0]["proposal"].(CoordinatorProposal); !ok {
		t.Fatalf("status data = %#v, want data.proposal", posted[0])
	}
	if !strings.Contains(summary, "Add a retry") {
		t.Fatalf("status summary = %q, want it to name the proposal", summary)
	}
}

func TestCoordinatorToolErrorsBecomeContent(t *testing.T) {
	t.Parallel()
	reader := &fakeCoordinatorReader{issuesErr: errors.New("hub unavailable")}
	toolset := NewCoordinatorToolset(reader, "prj_1", func(context.Context, map[string]any, string) error {
		return errors.New("conversation closed")
	}, nil)
	for _, test := range []struct {
		name      string
		tool      string
		arguments string
		want      string
	}{
		{"unknown tool", "delete_everything", `{}`, "unknown tool"},
		{"unknown field", coordinatorToolListAttention, `{"scope":"all_projects"}`, "invalid tool arguments"},
		{"trailing content", coordinatorToolListAttention, `{} {}`, "invalid tool arguments"},
		{"oversized arguments", coordinatorToolListAttention, `{"limit":` + strings.Repeat("0", coordinatorToolArgumentBytes) + `1}`, "invalid tool arguments"},
		{"read failure", coordinatorToolListAttention, `{}`, "hub unavailable"},
		{"missing work item", coordinatorToolExplainIssue, `{"work_item_id":"  "}`, "work_item_id is required"},
		{"empty title", coordinatorToolProposeIssue, `{"title":" ","objective":"o"}`, "title"},
		{"empty objective", coordinatorToolProposeIssue, `{"title":"t","objective":" "}`, "objective is required"},
		{"status post failure", coordinatorToolProposeIssue, `{"title":"t","objective":"o"}`, "conversation closed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := callCoordinatorTool(t, toolset, test.tool, test.arguments)
			if result.Success {
				t.Fatalf("%s succeeded, want a reported error", test.tool)
			}
			decoded := decodeCoordinatorResult(t, result)
			message, _ := decoded["error"].(string)
			if !strings.Contains(message, test.want) {
				t.Fatalf("error = %q, want it to mention %q", message, test.want)
			}
		})
	}
}

func TestCoordinatorToolReadFailuresBecomeContent(t *testing.T) {
	t.Parallel()
	issue := coordinatorTestIssue("wi_1", "Explain me", "Blocked", false)
	for _, test := range []struct {
		name   string
		reader *fakeCoordinatorReader
		tool   string
		want   string
	}{
		{
			// An issue the scan must check for a running attempt: a blocked
			// one is bucketed from the issue alone and reads nothing.
			name:   "attention attempts unavailable",
			reader: &fakeCoordinatorReader{issues: []tracker.NativeIssue{coordinatorTestIssue("wi_1", "Explain me", "In Review", false)}, attemptsErr: errors.New("attempts unavailable")},
			tool:   coordinatorToolListAttention,
			want:   "attempts unavailable",
		},
		{
			name:   "explain issue unavailable",
			reader: &fakeCoordinatorReader{issueErr: errors.New("issue unavailable")},
			tool:   coordinatorToolExplainIssue,
			want:   "issue unavailable",
		},
		{
			name:   "explain attempts unavailable",
			reader: &fakeCoordinatorReader{issues: []tracker.NativeIssue{issue}, attemptsErr: errors.New("attempts unavailable")},
			tool:   coordinatorToolExplainIssue,
			want:   "attempts unavailable",
		},
		{
			name:   "explain comments unavailable",
			reader: &fakeCoordinatorReader{issues: []tracker.NativeIssue{issue}, commentsErr: errors.New("comments unavailable")},
			tool:   coordinatorToolExplainIssue,
			want:   "comments unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			toolset := NewCoordinatorToolset(test.reader, "prj_1", nil, nil)
			arguments := `{"work_item_id":"wi_1"}`
			if test.tool == coordinatorToolListAttention {
				arguments = `{}`
			}
			result := callCoordinatorTool(t, toolset, test.tool, arguments)
			if result.Success {
				t.Fatalf("%s succeeded despite a failing hub read", test.tool)
			}
			decoded := decodeCoordinatorResult(t, result)
			if message, _ := decoded["error"].(string); !strings.Contains(message, test.want) {
				t.Fatalf("error = %q, want it to mention %q", message, test.want)
			}
		})
	}
}

func TestCoordinatorProposeIssueRejectsForeignProject(t *testing.T) {
	t.Parallel()
	posts := 0
	toolset := NewCoordinatorToolset(&fakeCoordinatorReader{}, "prj_1", func(context.Context, map[string]any, string) error {
		posts++
		return nil
	}, nil)
	result := callCoordinatorTool(t, toolset, coordinatorToolProposeIssue, `{"title":"t","objective":"o","project_id":"prj_other"}`)
	if result.Success || posts != 0 {
		t.Fatalf("a proposal targeting another project was accepted: %#v posts %d", result, posts)
	}
	decoded := decodeCoordinatorResult(t, result)
	if message, _ := decoded["error"].(string); !strings.Contains(message, "prj_other") {
		t.Fatalf("error = %q, want it to name the rejected project", message)
	}
}

func TestCoordinatorToolsetWithoutReader(t *testing.T) {
	t.Parallel()
	toolset := NewCoordinatorToolset(nil, "prj_1", nil, nil)
	for _, tool := range []string{coordinatorToolListAttention, coordinatorToolExplainIssue} {
		result := callCoordinatorTool(t, toolset, tool, `{"work_item_id":"wi_1"}`)
		if result.Success {
			t.Fatalf("%s succeeded without a hub reader", tool)
		}
	}
}

// A hub that answers with an empty page and still offers a cursor must not
// hold the tool in a loop that can never finish its scan.
func TestCoordinatorListAttentionStopsOnAnEmptyPage(t *testing.T) {
	t.Parallel()
	reader := &fakeCoordinatorReader{emptyPages: true}
	toolset := NewCoordinatorToolset(reader, "prj_1", nil, nil)
	result := callCoordinatorTool(t, toolset, coordinatorToolListAttention, `{}`)
	if !result.Success {
		t.Fatalf("list_attention failed: %s", result.Content)
	}
	if got := reader.queryCount(); got != 1 {
		t.Fatalf("issue pages fetched = %d, want the scan to stop at the first empty page", got)
	}
	decoded := decodeCoordinatorResult(t, result)
	if scanned, _ := decoded["scanned"].(float64); scanned != 0 {
		t.Fatalf("scanned = %v, want 0", decoded["scanned"])
	}
}

// The scan reads attempts only where a running attempt could change the
// answer, and never more than its budget: one tool call must not turn into a
// hub read per issue.
func TestCoordinatorListAttentionBoundsAttemptReads(t *testing.T) {
	t.Parallel()
	reader := &fakeCoordinatorReader{}
	for i := range coordinatorAttentionScan {
		reader.issues = append(reader.issues, coordinatorTestIssue("wi_"+strconv.Itoa(i), "Open", "In Review", false))
	}
	blocked := coordinatorTestIssue("wi_blocked", "Blocked", "Blocked", false)
	blocked.Blockers = []tracker.NativeDependency{{ID: "wi_dep", Terminal: false}}
	reader.issues[0] = blocked
	toolset := NewCoordinatorToolset(reader, "prj_1", nil, nil)

	result := callCoordinatorTool(t, toolset, coordinatorToolListAttention, `{}`)
	if !result.Success {
		t.Fatalf("list_attention failed: %s", result.Content)
	}
	reads := reader.attemptReads()
	if reads > coordinatorAttentionAttemptReads {
		t.Fatalf("attempt reads = %d, want at most %d", reads, coordinatorAttentionAttemptReads)
	}
	if reads == 0 {
		t.Fatal("attempt reads = 0, want the scan to still look for running work")
	}
	decoded := decodeCoordinatorResult(t, result)
	items, _ := decoded["blocked"].([]any)
	if len(items) != 1 {
		t.Fatalf("blocked = %v, want the blocked issue", decoded["blocked"])
	}
	first, _ := items[0].(map[string]any)
	if id, _ := first["work_item_id"].(string); id != "wi_blocked" {
		t.Fatalf("blocked item = %v, want wi_blocked", first)
	}
	if truncated, _ := decoded["truncated"].(bool); !truncated {
		t.Fatal("truncated = false, want the spent attempt budget reported")
	}
}
