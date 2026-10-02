package hubclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeExecutionReadToolsKeepHostAuthority(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Read current native evidence")
	h.claim(t, issue.ID)
	item := tracker.NativeWorkItemID(issue.ID)
	if _, err := h.admin.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: nativeMutationKey(), Body: "Genuine native discussion"}); err != nil {
		t.Fatal(err)
	}
	change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Existing implementation"})
	if err != nil {
		t.Fatal(err)
	}
	version := h.publish(t, item, change.ID, strings.Repeat("a", 40))
	if _, err := h.admin.DiscussChange(t.Context(), item, change.ID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: version.ID, Body: "Current version finding"}); err != nil {
		t.Fatal(err)
	}
	var foreignProject tracker.NativeProject
	if err := h.admin.client.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/projects", map[string]any{"name": "foreign", "idempotency_key": "foreign-project", "states": []tracker.NativeState{{Name: "Backlog"}}}, &foreignProject); err != nil {
		t.Fatal(err)
	}
	foreign, err := h.admin.client.Native(h.organization, foreignProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreignIssue, err := foreign.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Foreign private instruction", Body: "Never reveal this foreign instruction", State: "Backlog"})
	if err != nil {
		t.Fatal(err)
	}
	execution := h.scheduler.RunExecution(issue.ID)
	source, ok := execution.(runner.ToolExecution)
	if !ok {
		t.Fatal("native execution omitted its read tools")
	}
	tools, handler := source.AgentTools()
	if len(tools) != 5 {
		t.Fatalf("read tools = %d, want 5", len(tools))
	}
	for _, test := range []struct {
		name  string
		extra map[string]any
		want  string
	}{
		{operatortool.WorkItem, map[string]any{"reference": issue.ID}, issue.Title},
		{operatortool.WorkComments, map[string]any{"reference": issue.ID, "limit": 1}, "Genuine native discussion"},
		{operatortool.WorkHistory, map[string]any{"reference": issue.ID, "limit": 1}, "next_cursor"},
		{operatortool.ListChanges, map[string]any{"work_item_id": issue.ID, "limit": 1}, change.ID},
		{operatortool.GetChange, map[string]any{"work_item_id": issue.ID, "change_id": change.ID}, version.HeadSHA},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.extra["project_id"] = string(h.project)
			arguments, err := json.Marshal(test.extra)
			if err != nil {
				t.Fatal(err)
			}
			result, err := handler(t.Context(), runner.AgentToolCall{Name: test.name, Arguments: arguments})
			if err != nil || !result.Success || !strings.Contains(result.Content, test.want) {
				t.Fatalf("read success=%t error=%v, expected genuine evidence %q", result.Success, err, test.want)
			}
			if strings.Contains(result.Content, nativeChangeAdminToken) || strings.Contains(result.Content, h.native.client.tokenSource()) || strings.Contains(result.Content, "Bearer ") {
				t.Fatal("read exposed a host credential")
			}
		})
	}
	for _, test := range []struct {
		name string
		args map[string]any
		want error
	}{
		{operatortool.WorkItem, map[string]any{"project_id": foreignProject.ID, "reference": foreignIssue.WorkItemID}, operatortool.ErrAccessDenied},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": foreignIssue.WorkItemID}, operatortool.ErrAccessDenied},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": issue.ID, "url": "https://foreign.invalid"}, operatortool.ErrInvalidArguments},
		{operatortool.WorkComments, map[string]any{"project_id": h.project, "reference": issue.ID, "limit": 201}, operatortool.ErrInvalidArguments},
		{operatortool.CreateChange, map[string]any{"project_id": h.project, "work_item_id": issue.ID}, operatortool.ErrUnknownTool},
	} {
		arguments, err := json.Marshal(test.args)
		if err != nil {
			t.Fatal(err)
		}
		result, err := handler(t.Context(), runner.AgentToolCall{Name: test.name, Arguments: arguments})
		if !errors.Is(err, test.want) || result.Success {
			t.Fatalf("refusal %s: success=%t error=%v, want %v", test.name, result.Success, err, test.want)
		}
	}
	h.scheduler.mu.Lock()
	delete(h.scheduler.nativeClaims, issue.ID)
	h.scheduler.mu.Unlock()
	arguments, err := json.Marshal(map[string]any{"project_id": h.project, "reference": issue.ID})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkItem, Arguments: arguments})
	if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || result.Success {
		t.Fatalf("lost execution authority: success=%t error=%v", result.Success, err)
	}
}
