package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestNativeExecutionReadToolsKeepHostAuthority(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	h := newNativeChangeHub(t, true)
	h.admin.client.baseURL.Scheme = "https"
	transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != h.admin.client.baseURL.Host {
			return nil, errors.New("unexpected native read destination")
		}
		return h.admin.client.httpClient.Transport.RoundTrip(request)
	})
	previousTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
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
	client, err := New(Config{URL: file.HubURL, IdentityFile: identityPath, HTTPClient: &http.Client{Transport: transport}})
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
	selected, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Runner admission context", Body: "private-admission-body" + issueContractTestSections, State: "Todo", Labels: []string{"selected-label"}})
	if err != nil {
		t.Fatal(err)
	}
	largeSelectors := make([]string, 33)
	for i := range largeSelectors {
		largeSelectors[i] = "label-" + strconv.Itoa(i)
	}
	for _, test := range []struct {
		name    string
		exclude []string
		outcome string
	}{
		{name: "large legitimate selector list keeps heartbeat and unknown evidence", exclude: largeSelectors, outcome: "unknown"},
		{name: "long legitimate selector keeps heartbeat and unknown evidence", exclude: []string{strings.Repeat("a", 129)}, outcome: "unknown"},
		{name: "registered runner selectors refuse", exclude: []string{"selected-label"}, outcome: "skipped"},
		{name: "registered runner selectors admit", outcome: "ready"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := h.scheduler.Heartbeat(t.Context()); err != nil {
				t.Fatal(err)
			}
			observed := time.Now().UTC()
			h.scheduler.ObserveNativeAdmission("local", tracker.NativeAdmissionContext{ObservedAt: observed, PolicyID: h.descriptor.ID, WorkflowStates: []string{"Todo"}, LabelExclude: test.exclude})
			h.scheduler.mu.Lock()
			h.scheduler.nativeHeartbeats[h.project] = time.Now().Add(-2 * time.Second)
			h.scheduler.mu.Unlock()
			if err := h.scheduler.Heartbeat(t.Context()); err != nil {
				t.Fatal(err)
			}
			evidence, err := h.admin.RuntimeEvidence(t.Context(), selected.WorkItemID, "")
			if err != nil || len(evidence.Admission) != 1 || evidence.Admission[0].Outcome != test.outcome || evidence.Admission[0].RunnerID != file.Identity.RunnerID || evidence.LatestDecision != nil || evidence.Issue.Body != "" {
				t.Fatalf("native runtime dropped current runner admission context: %#v, %v", evidence, err)
			}
			if test.outcome == "unknown" {
				if evidence.Admission[0].SelectorObservedAt != nil {
					t.Fatalf("unrepresentable selectors were truncated into evidence: %#v", evidence)
				}
			} else if evidence.Admission[0].SelectorSource != "registered_runner_heartbeat" || evidence.Admission[0].SelectorObservedAt == nil || !evidence.Admission[0].SelectorObservedAt.Equal(observed) {
				t.Fatalf("heartbeat observation identity lost: %#v", evidence)
			}
		})
	}
	if _, err := h.admin.SetArchived(t.Context(), selected.WorkItemID, selected.Revision, true, nativeMutationKey()); err != nil {
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
	for i := range 6 {
		version = h.publish(t, item, change.ID, strings.Repeat(strconv.Itoa(i+1), 40), version.ID)
	}
	for range 9 {
		if _, err := h.admin.DiscussChange(t.Context(), item, change.ID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: version.ID, Body: strings.Repeat("é<\n", 5000)}); err != nil {
			t.Fatal(err)
		}
	}
	fullChange, err := h.admin.Change(t.Context(), item, change.ID)
	if err != nil {
		t.Fatal(err)
	}
	fullBytes, err := json.Marshal(fullChange)
	if err != nil || len(fullBytes) <= operatortool.MaxResultBytes {
		t.Fatalf("native aggregate did not exceed budget: bytes=%d %v", len(fullBytes), err)
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
	if _, err := foreign.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Another foreign item", State: "Backlog"}); err != nil {
		t.Fatal(err)
	}
	foreignPage, err := foreign.Issues(t.Context(), operatortool.WorkReadRequest{Limit: 1}.NativeWorkQuery())
	if err != nil || foreignPage.NextCursor == "" {
		t.Fatalf("foreign page = %#v, %v", foreignPage, err)
	}
	discovered := make(map[tracker.NativeWorkItemID]string)
	for i, fingerprint := range []string{"native-search-existing-one", "native-search-existing-two"} {
		body := issueorigin.Stamp("Authoritative existing problem", issueorigin.Origin{Kind: "worker", Source: "existing-owner", Fingerprint: fingerprint})
		item, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Search fixture " + strconv.Itoa(i), Body: body, State: "Todo", Priority: new(0), Labels: []string{"search-fixture"}, Assignees: []string{"operator"}})
		if err != nil {
			t.Fatal(err)
		}
		discovered[item.WorkItemID] = fingerprint
	}
	execution := h.scheduler.RunExecution(issue.ID)
	owner := execution.(*nativeExecution)
	if err := owner.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "merge", Backend: "git", Model: "none"}); err != nil {
		t.Fatal(err)
	}
	source, ok := execution.(runner.ToolExecution)
	if !ok {
		t.Fatal("native execution omitted its read tools")
	}
	tools, handler := source.AgentTools()
	if len(tools) != 13 {
		t.Fatalf("native tools = %d, want 13", len(tools))
	}
	diagnosticArgs, err := json.Marshal(map[string]any{"project_id": h.project, "runner_id": file.Identity.RunnerID, "issue_id": issue.ID})
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	nativeTransport := h.scheduler.client.httpClient.Transport
	h.scheduler.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			writes++
		}
		return nativeTransport.RoundTrip(request)
	})
	diagnosticResult, diagnosticErr := handler(t.Context(), runner.AgentToolCall{Name: operatortool.RunnerProjectDiagnostics, Arguments: diagnosticArgs})
	h.scheduler.client.httpClient.Transport = nativeTransport
	var diagnosticPage runnerauth.DiagnosticPage
	if diagnosticErr != nil || !diagnosticResult.Success || json.Unmarshal([]byte(diagnosticResult.Content), &diagnosticPage) != nil || diagnosticPage.ProjectID != string(h.project) || diagnosticPage.RunnerID != file.Identity.RunnerID || writes != 0 {
		t.Fatalf("worker diagnostic=%s error=%v writes=%d", diagnosticResult.Content, diagnosticErr, writes)
	}
	listArgs, err := json.Marshal(map[string]any{"project_id": h.project, "query": "native-search-existing", "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[tracker.NativeWorkItemID]bool)
	var cursor string
	for range 2 {
		args, err := json.Marshal(map[string]any{"project_id": h.project, "query": "native-search-existing", "limit": 1, "cursor": cursor})
		if err != nil {
			t.Fatal(err)
		}
		result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkList, Arguments: args})
		var page operatortool.WorkReadResult[operatortool.NativeWorkPage]
		if err != nil || !result.Success || json.Unmarshal([]byte(result.Content), &page) != nil || page.ProjectID != string(h.project) || page.GeneratedAt.IsZero() || page.Freshness != "available" || len(page.Data.Items) != 1 {
			t.Fatalf("bounded project search = %s, %v", result.Content, err)
		}
		item := page.Data.Items[0]
		if discovered[item.WorkItemID] == "" || seen[item.WorkItemID] || item.ProjectID != h.project || item.Body != "" || !strings.Contains(result.Content, `"omitted_fields":["body"`) {
			t.Fatalf("search returned an unscoped, repeated or unbounded item: %s", result.Content)
		}
		seen[item.WorkItemID] = true
		cursor = page.Data.NextCursor
		if (cursor != "") != (len(seen) == 1) {
			t.Fatalf("search cursor lost page boundary: %s", result.Content)
		}
		args, err = json.Marshal(map[string]any{"project_id": h.project, "reference": item.WorkItemID})
		if err != nil {
			t.Fatal(err)
		}
		result, err = handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkItem, Arguments: args})
		var detail operatortool.WorkReadResult[operatortool.NativeItem]
		if err != nil || !result.Success || json.Unmarshal([]byte(result.Content), &detail) != nil {
			t.Fatalf("discovered work item = %s, %v", result.Content, err)
		}
		origin, ok := issueorigin.Parse(detail.Data.Body)
		if !ok || origin.Fingerprint != discovered[item.WorkItemID] || origin.Kind != "worker" || origin.Source != "existing-owner" {
			t.Fatalf("discovery lost authoritative fingerprint: %#v", origin)
		}
	}
	receiptArgs, err := json.Marshal(map[string]any{"project_id": h.project, "reference": issue.ID, "native_attempt_id": owner.data.AttemptID})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkAttemptReceipt, Arguments: receiptArgs})
	var receipt operatortool.WorkReadResult[tracker.NativeRuntimeEvidence]
	if err != nil || !result.Success || json.Unmarshal([]byte(result.Content), &receipt) != nil || receipt.Data.Attempt == nil || receipt.Data.Attempt.Runtime == nil || receipt.Data.Attempt.Runtime.Activity == nil {
		t.Fatalf("git-only activity receipt unavailable: %s, err=%v", result.Content, err)
	}
	if p := receipt.Data.Attempt.Runtime.Activity; p.SessionID != 0 || p.Summary == nil || len(p.Spans) != 0 || p.Breakdown().ObservedSeconds != 0 || !receipt.Data.Attempt.Runtime.Identity.IsZero() {
		t.Fatalf("git-only receipt fabricated provider activity: %+v", p)
	}
	t.Run("bounded native Change traversal", func(t *testing.T) {
		seen := map[string]bool{}
		cursor := ""
		for pages := range 20 {
			arguments, err := json.Marshal(operatortool.ChangeArguments{ProjectID: string(h.project), ItemID: string(item), ChangeID: change.ID, Section: "discussion", Cursor: cursor, Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.GetChange, Arguments: arguments})
			var value operatortool.ChangeResult
			if err != nil || !result.Success || len(result.Content) > operatortool.MaxResultBytes || json.Unmarshal([]byte(result.Content), &value) != nil || value.Read == nil || value.Read.VersionID != version.ID || len(value.Detail.Discussion) > 2 || value.ValidationAudit == nil || value.Read.Sections["versions"].NextCursor == "" {
				t.Fatalf("unbounded or incomplete native Change: bytes=%d %v", len(result.Content), err)
			}
			for _, discussion := range value.Detail.Discussion {
				if seen[discussion.ID] || discussion.VersionID != version.ID {
					t.Fatal("duplicate or foreign-version discussion")
				}
				seen[discussion.ID] = true
			}
			page := value.Read.Sections["discussion"]
			if page.Complete {
				break
			}
			if page.NextCursor == "" || page.NextCursor == cursor || pages == 19 {
				t.Fatal("native Change continuation did not progress")
			}
			cursor = page.NextCursor
		}
		if len(seen) != 9 {
			t.Fatalf("lost native evidence identities: %d", len(seen))
		}
		if _, err := h.admin.DiscussChange(t.Context(), item, change.ID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: version.ID, Body: "late immutable evidence"}); err != nil {
			t.Fatal(err)
		}
		arguments, err := json.Marshal(operatortool.ChangeArguments{ProjectID: string(h.project), ItemID: string(item), ChangeID: change.ID, Section: "discussion", Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.GetChange, Arguments: arguments})
		if err != nil || strings.Contains(result.Content, "late immutable evidence") {
			t.Fatalf("continuation included evidence appended after snapshot: %v", err)
		}
		if _, err := h.admin.DiscussChange(t.Context(), item, change.ID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: version.ID, Body: strings.Repeat("\x00", 64000)}); err != nil {
			t.Fatal(err)
		}
		cursor = ""
		for pages := range 20 {
			arguments, err := json.Marshal(operatortool.ChangeArguments{ProjectID: string(h.project), ItemID: string(item), ChangeID: change.ID, Section: "discussion", Cursor: cursor, Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.GetChange, Arguments: arguments})
			if err != nil {
				var refusal operatortool.RequestError
				if result.Success || json.Unmarshal([]byte(result.Content), &refusal) != nil || refusal.Code != "change_record_too_large" || !strings.Contains(refusal.Message, "operator repair") || strings.Contains(result.Content, `\u0000`) {
					t.Fatalf("native oversized refusal lost safe guidance: %s %v", result.Content, err)
				}
				break
			}
			var value operatortool.ChangeResult
			if json.Unmarshal([]byte(result.Content), &value) != nil || value.Read.Sections["discussion"].Complete || pages == 19 {
				t.Fatal("oversized native record was silently omitted or retried indefinitely")
			}
			cursor = value.Read.Sections["discussion"].NextCursor
		}
	})

	at := time.Now().UTC().Add(-2 * time.Second)
	observation := tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 2, Phase: "merging", HeartbeatAt: at, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 42, Generation: 2, Stage: "merge", Status: "running", Coverage: "partial", StartedAt: at, AsOf: at, Spans: []workflowmetrics.ActivitySpan{{ID: "git", Kind: "implementation", Outcome: "running", StartedAt: at}}}}
	if err := owner.ObserveRuntime(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	observation.Phase = "validation"
	observation.HeartbeatAt = at.Add(time.Second)
	if err := owner.ObserveRuntime(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	other, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Another item", Body: issueContractTestSections, State: "Todo"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Name == operatortool.WorkList && (!strings.Contains(tool.Description, "Use cursor, not offset") || strings.Contains(tool.Description, "reference must")) {
			t.Fatal("project search advertised an unsupported paging or reference contract")
		}
		if tool.Name == operatortool.WorkItem && !strings.Contains(tool.Description, "canonical native work-item ID") {
			t.Fatal("read tool advertised an unsupported reference contract")
		}
	}
	for _, test := range []struct {
		name  string
		extra map[string]any
		want  string
	}{
		{operatortool.WorkList, map[string]any{"fingerprint": "native-search-existing-one", "open": true, "limit": 1}, "Search fixture 0"},
		{operatortool.WorkList, map[string]any{"fingerprint": "native-search-existing", "open": true, "limit": 1}, `"items":[]`},
		{operatortool.WorkList, map[string]any{"query": "native-search-existing", "open": false, "limit": 1}, `"items":[]`},
		{operatortool.WorkList, map[string]any{"query": "native-search-existing", "state": "Todo", "label": "search-fixture", "assignee": "operator", "priority": 0, "limit": 1}, "Search fixture"},
		{operatortool.WorkList, map[string]any{"query": "Search fixture", "states": []string{"In Review", "Todo"}, "labels": []string{"search-fixture", "absent"}, "assignees": []string{"operator", "absent"}, "priorities": []int{0, 3}, "archived": "all", "include": []string{"work", "workspace"}, "limit": 1}, `"lanes"`},
		{operatortool.WorkList, map[string]any{"archived": "true", "limit": 1}, selected.Title},
		{operatortool.WorkList, map[string]any{"query": "no-matching-work-item", "limit": 1}, `"items":[]`},
		{operatortool.WorkItem, map[string]any{"reference": issue.ID}, issue.Title},
		{operatortool.WorkComments, map[string]any{"reference": issue.ID, "limit": 1}, "Genuine native discussion"},
		{operatortool.WorkHistory, map[string]any{"reference": issue.ID, "limit": 1}, "next_cursor"},
		{operatortool.ListChanges, map[string]any{"work_item_id": issue.ID, "limit": 1}, change.ID},
		{operatortool.GetChange, map[string]any{"work_item_id": issue.ID, "change_id": change.ID}, version.HeadSHA},
		{operatortool.WorkRuns, map[string]any{"reference": issue.ID, "limit": 1}, owner.data.AttemptID},
		{operatortool.BoardActivity, map[string]any{"reference": issue.ID, "limit": 1}, "next_cursor"},
		{operatortool.WorkAttemptReceipt, map[string]any{"reference": issue.ID, "native_attempt_id": owner.data.AttemptID}, "validation"},
		{operatortool.WorkAttemptReceipt, map[string]any{"reference": issue.ID, "attempt_id": 42}, "validation"},
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
			if test.name == operatortool.WorkAttemptReceipt {
				var receipt operatortool.WorkReadResult[tracker.NativeRuntimeEvidence]
				if err := json.Unmarshal([]byte(result.Content), &receipt); err != nil {
					t.Fatal(err)
				}
				attempt := receipt.Data.Attempt
				if receipt.GeneratedAt.IsZero() || receipt.GeneratedAt != receipt.Data.ObservedAt || attempt == nil || attempt.AttemptID != owner.data.AttemptID || attempt.RuntimeFreshness != "available" || attempt.Runtime == nil || len(attempt.Runtime.Phases) != 2 || attempt.Runtime.Phases[0].FinishedAt.IsZero() || attempt.Runtime.Activity == nil || len(attempt.Runtime.Activity.Spans) != 0 {
					t.Fatalf("receipt lost bounded phase/freshness evidence: %s", result.Content)
				}
			}
		})
	}
	for _, test := range []struct {
		name string
		args map[string]any
		want error
	}{
		{operatortool.WorkList, map[string]any{"project_id": foreignProject.ID}, operatortool.ErrAccessDenied},
		{operatortool.WorkList, map[string]any{"project_id": h.project, "cursor": foreignPage.NextCursor}, operatortool.ErrInvalidArguments},
		{operatortool.WorkList, map[string]any{"project_id": h.project, "cursor": "unsupported-cursor"}, operatortool.ErrInvalidArguments},
		{operatortool.WorkList, map[string]any{"project_id": h.project, "offset": 1}, operatortool.ErrInvalidArguments},
		{operatortool.WorkList, map[string]any{"project_id": h.project, "limit": 201}, operatortool.ErrInvalidArguments},
		{operatortool.WorkList, map[string]any{"project_id": h.project, "fingerprint": 42}, operatortool.ErrInvalidArguments},
		{operatortool.WorkList, map[string]any{"project_id": h.project, "include": []string{"credentials"}}, operatortool.ErrInvalidArguments},
		{operatortool.WorkList, map[string]any{"project_id": h.project, "organization_id": "org_foreign"}, operatortool.ErrInvalidArguments},
		{operatortool.WorkList, map[string]any{"project_id": h.project, "reference": issue.ID}, operatortool.ErrInvalidArguments},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": "195"}, operatortool.ErrInvalidArguments},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": "Read current native evidence"}, operatortool.ErrInvalidArguments},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": "https://cloud.detent.build/work/195"}, operatortool.ErrInvalidArguments},
		{operatortool.WorkItem, map[string]any{"project_id": foreignProject.ID, "reference": foreignIssue.WorkItemID}, operatortool.ErrAccessDenied},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": foreignIssue.WorkItemID}, operatortool.ErrAccessDenied},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": issue.ID, "url": "https://foreign.invalid"}, operatortool.ErrInvalidArguments},
		{operatortool.WorkComments, map[string]any{"project_id": h.project, "reference": issue.ID, "limit": 201}, operatortool.ErrInvalidArguments},
		{operatortool.CreateChange, map[string]any{"project_id": h.project, "work_item_id": issue.ID}, operatortool.ErrUnknownTool},
		{operatortool.AppUpdates, map[string]any{}, operatortool.ErrUnknownTool},
		{operatortool.WorkAttemptReceipt, map[string]any{"project_id": foreignProject.ID, "reference": foreignIssue.WorkItemID, "native_attempt_id": owner.data.AttemptID}, operatortool.ErrAccessDenied},
		{operatortool.WorkAttemptReceipt, map[string]any{"project_id": h.project, "reference": foreignIssue.WorkItemID, "native_attempt_id": owner.data.AttemptID}, operatortool.ErrAccessDenied},
		{operatortool.WorkAttemptReceipt, map[string]any{"project_id": h.project, "reference": other.WorkItemID, "native_attempt_id": owner.data.AttemptID}, operatortool.ErrAccessDenied},
		{operatortool.WorkAttemptReceipt, map[string]any{"project_id": h.project, "reference": other.WorkItemID, "attempt_id": 42}, operatortool.ErrAccessDenied},
		{operatortool.WorkAttemptReceipt, map[string]any{"project_id": h.project, "reference": issue.ID}, operatortool.ErrInvalidArguments},
		{operatortool.BoardActivity, map[string]any{"project_id": h.project, "reference": issue.ID, "limit": 201}, operatortool.ErrInvalidArguments},
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
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	result, err = handler(cancelled, runner.AgentToolCall{Name: operatortool.WorkList, Arguments: listArgs})
	if !errors.Is(err, context.Canceled) || result.Success {
		t.Fatalf("cancelled search authority: success=%t error=%v", result.Success, err)
	}
	if err := h.native.Release(t.Context(), owner.claim.lease, "cancelled"); err != nil {
		t.Fatal(err)
	}
	result, err = handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkList, Arguments: listArgs})
	if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || result.Success {
		t.Fatalf("lost search authority: success=%t error=%v", result.Success, err)
	}
	result, err = handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkAttemptReceipt, Arguments: receiptArgs})
	if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || result.Success {
		t.Fatalf("lost execution authority: success=%t error=%v", result.Success, err)
	}
	for _, name := range []string{operatortool.ReadAttachmentMetadata, operatortool.ReadAttachment} {
		arguments, err := json.Marshal(map[string]string{"project_id": string(h.project), "attachment_id": "att_" + strings.Repeat("0", 32)})
		if err != nil {
			t.Fatal(err)
		}
		result, err := handler(t.Context(), runner.AgentToolCall{Name: name, Arguments: arguments})
		if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || result.Success {
			t.Fatalf("lost attachment read authority: success=%t error=%v", result.Success, err)
		}
	}
}
