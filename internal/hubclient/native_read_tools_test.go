package hubclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeExecutionReadToolsKeepHostAuthority(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t)
	issue := h.createInProgress(t, "Read current native evidence")
	identityPath := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(identityPath, h.admin.client.baseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, ID: file.Identity.MachineID, Hostname: "read-tools", DisplayName: "Read tool runner", Capacity: 1, Version: "test"}
	if _, err := EnrollRunner(t.Context(), identityPath, h.organization, enrollment.Token, machine); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: file.HubURL, IdentityFile: identityPath})
	if err != nil {
		t.Fatal(err)
	}
	if h.native, err = client.Native(h.organization, h.project); err != nil {
		t.Fatal(err)
	}
	h.scheduler, err = NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h.claim(t, issue.ID)
	file, err = runnerauth.Load(identityPath)
	if err != nil {
		t.Fatal(err)
	}
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
	for _, tool := range tools {
		if tool.Name == operatortool.WorkItem && !strings.Contains(tool.Description, "canonical native work-item ID") {
			t.Fatal("read tool advertised an unsupported reference contract")
		}
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
			if strings.Contains(result.Content, nativeChangeAdminToken) || strings.Contains(result.Content, file.Credential) || strings.Contains(result.Content, "Bearer ") {
				t.Fatal("read exposed a host credential")
			}
		})
	}
	for _, test := range []struct {
		name string
		args map[string]any
		want error
	}{
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": "195"}, operatortool.ErrInvalidArguments},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": "Read current native evidence"}, operatortool.ErrInvalidArguments},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": "https://cloud.detent.build/work/195"}, operatortool.ErrInvalidArguments},
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
