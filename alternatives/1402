package hubclient

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestNativeCommentOffsetRefusal(t *testing.T) {
	client := NativeClient{project: "prj_example"}
	for _, offset := range []int{1, 100} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			arguments, err := json.Marshal(map[string]any{"project_id": "prj_example", "reference": "wi_example", "offset": offset, "limit": 100})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.readAgentTool(t.Context(), runner.AgentToolCall{Name: operatortool.WorkComments, Arguments: arguments}, time.Now())
			var refusal *operatortool.RequestError
			if !errors.As(err, &refusal) || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, "returned next_cursor") {
				t.Fatalf("native offset refusal lacks paging guidance: %v", err)
			}
		})
	}
}

func TestNativeCommentReadFallback(t *testing.T) {
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Oversized comment history")
	h.claim(t, issue.ID)
	item := tracker.NativeWorkItemID(issue.ID)
	expected := make([]tracker.NativeComment, 5)
	for index := range expected {
		body := "readable comment"
		if index > 0 {
			body = strings.Repeat("\x00", (64<<10)-1) + strconv.Itoa(index)
		}
		comment, err := h.admin.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: nativeMutationKey(), Body: body})
		if err != nil {
			t.Fatal(err)
		}
		expected[index] = comment
	}
	execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	tools, handler := execution.AgentTools()
	if !slices.ContainsFunc(tools, func(tool runner.AgentTool) bool { return tool.Name == operatortool.WorkBulkExport }) {
		t.Error("native runner lacks the advertised oversized-comment fallback")
	}
	call := func(name string, arguments map[string]any) (runner.AgentToolResult, error) {
		t.Helper()
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		return handler(t.Context(), runner.AgentToolCall{Name: name, Arguments: raw})
	}
	arguments := map[string]any{"project_id": h.project, "reference": item, "limit": 100}
	result, err := call(operatortool.WorkComments, arguments)
	var page operatortool.WorkReadResult[tracker.Page[tracker.NativeComment]]
	if err != nil || !result.Success || json.Unmarshal([]byte(result.Content), &page) != nil || len(page.Data.Items) != 1 || page.Data.NextCursor == "" || !reflect.DeepEqual(page.Data.Items[0], expected[0]) {
		t.Fatalf("readable comment page failed: success=%t err=%v", result.Success, err)
	}
	arguments["cursor"] = page.Data.NextCursor
	result, err = call(operatortool.WorkComments, arguments)
	var refusal *operatortool.RequestError
	var content operatortool.RequestError
	if !errors.As(err, &refusal) || result.Success || json.Unmarshal([]byte(result.Content), &content) != nil || content.Code != "invalid_request" || !strings.Contains(content.Message, "single comment") || !strings.Contains(content.Message, operatortool.WorkBulkExport) {
		t.Errorf("native runner lost oversized-comment guidance: content=%s err=%v", result.Content, err)
	}
	result, err = call(operatortool.WorkBulkExport, map[string]any{"project_id": h.project, "limit": 1})
	var exported operatortool.WorkBulkExportPage
	if err != nil || !result.Success || json.Unmarshal([]byte(result.Content), &exported) != nil || len(result.Content) <= maxResponseBytes || len(result.Content) > operatortool.WorkBulkExportMaxBytes || exported.ProjectID != string(h.project) || len(exported.Items) != 1 || !reflect.DeepEqual(exported.Items[0].Comments, expected) {
		t.Fatalf("native oversized-comment fallback failed: success=%t bytes=%d err=%v", result.Success, len(result.Content), err)
	}
	t.Logf("recovered %d complete comments in %d export bytes", len(expected), len(result.Content))
	for _, test := range []struct {
		name string
		want error
	}{
		{"foreign page", operatortool.ErrAccessDenied},
		{"foreign item project", operatortool.ErrAccessDenied},
		{"foreign item organization", operatortool.ErrAccessDenied},
		{"over byte budget", operatortool.ErrReadUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := exported
			page.Items = slices.Clone(exported.Items)
			switch test.name {
			case "foreign page":
				page.ProjectID = "prj_foreign"
			case "foreign item project":
				page.Items[0].ProjectID = "prj_foreign"
			case "foreign item organization":
				page.Items[0].OrganizationID = "org_foreign"
			}
			payload, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "over byte budget" {
				payload = append(payload, strings.Repeat(" ", operatortool.WorkBulkExportMaxBytes+1-len(payload))...)
			}
			client := execution.claim.source.client.client.httpClient
			transport := client.Transport
			client.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/work-items/export") {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(payload))), Request: request}, nil
				}
				return transport.RoundTrip(request)
			})
			t.Cleanup(func() { client.Transport = transport })
			result, err := call(operatortool.WorkBulkExport, map[string]any{"project_id": h.project, "limit": 1})
			if !errors.Is(err, test.want) || result.Success {
				t.Fatalf("invalid export was accepted: success=%t err=%v", result.Success, err)
			}
		})
	}
	for _, name := range []string{operatortool.WorkComments, operatortool.WorkBulkExport} {
		arguments := map[string]any{"project_id": "prj_foreign"}
		if name == operatortool.WorkComments {
			arguments["reference"] = item
		}
		result, err := call(name, arguments)
		if !errors.Is(err, operatortool.ErrAccessDenied) || result.Success {
			t.Fatalf("%s crossed project scope: %v", name, err)
		}
	}
}

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
	selected := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Runner admission context", Body: "private-admission-body" + issueContractTestSections, State: "Todo", Labels: []string{"selected-label"}})
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
	version := h.publish(t, item, change.ID, nativeChangeFixtureHead("b"))
	if _, err := h.admin.DiscussChange(t.Context(), item, change.ID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: version.ID, Body: "Current version finding"}); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"c", "d", "e", "f", "g", "h"} {
		version = h.publish(t, item, change.ID, nativeChangeFixtureHead(label), version.ID)
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
		body := issueorigin.Stamp("Authoritative existing problem"+issueContractTestSections, issueorigin.Origin{Kind: "worker", Source: "existing-owner", Fingerprint: fingerprint})
		item := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Search fixture " + strconv.Itoa(i), Body: body, State: "Todo", Priority: new(0), Labels: []string{"search-fixture"}, Assignees: []string{"operator"}})
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
	if len(tools) != 14 {
		t.Fatalf("native tools = %d, want 14", len(tools))
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
	listArgs, err := json.Marshal(map[string]any{"project_id": h.project, "query": "native-search-existing", "label": "search-fixture", "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[tracker.NativeWorkItemID]bool)
	var cursor string
	for range 2 {
		args, err := json.Marshal(map[string]any{"project_id": h.project, "query": "native-search-existing", "label": "search-fixture", "limit": 1, "cursor": cursor})
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
		for range 16 {
			if _, err := h.admin.DiscussChange(t.Context(), item, change.ID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: fullChange.Versions[0].ID, Body: strings.Repeat("historical feedback\n", 3000)}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := h.admin.Change(t.Context(), item, change.ID); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "response exceeds 1048576 bytes") {
			t.Fatalf("canonical aggregate did not reproduce the runtime transport limit: %v", err)
		}
		runtime, err := h.native.currentChange(t.Context(), item)
		if err != nil || runtime.Change.CurrentVersion != version.ID || len(runtime.Versions) != len(fullChange.Versions) || len(runtime.Discussion) != 9 {
			t.Fatalf("runtime lost source history or current feedback: versions=%d discussion=%d error=%v", len(runtime.Versions), len(runtime.Discussion), err)
		}
		for i := range runtime.Versions {
			if runtime.Versions[i].ID != fullChange.Versions[i].ID {
				t.Fatal("runtime version history changed order or identity")
			}
		}
		seen := map[string]bool{}
		cursor := ""
		for pages := range 20 {
			arguments, err := json.Marshal(operatortool.ChangeArguments{ProjectID: string(h.project), ItemID: string(item), ChangeID: change.ID, Section: "discussion", Cursor: cursor, Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.GetChange, Arguments: arguments})
			var value operatortool.ChangeResult
			if err != nil || !result.Success || len(result.Content) > operatortool.MaxResultBytes || json.Unmarshal([]byte(result.Content), &value) != nil || value.Read == nil || value.Read.VersionID != version.ID || len(value.Detail.Discussion) > 2 || value.Read.Sections["versions"].NextCursor == "" {
				t.Fatalf("unbounded or incomplete native Change: bytes=%d %v", len(result.Content), err)
			}
			if value.ValidationAudit != nil || !value.Read.PolicyDetailsOmitted {
				t.Fatal("native Change repeated project audit or omitted policy details without disclosure")
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
	observation.Validation = &gate.CommandResult{Command: "focused test", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), DurationNS: 1, Output: strings.Repeat("x", 64<<10)}
	if err := owner.ObserveRuntime(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	observation.Phase = "validation"
	observation.HeartbeatAt = at.Add(time.Second)
	if err := owner.ObserveRuntime(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	other := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Another item", Body: issueContractTestSections, State: "Todo"})
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
		{operatortool.WorkList, map[string]any{"fingerprint": "native-search-existing-one", "query": "fixture", "state": "Todo", "label": "search-fixture", "priority": 0, "open": true, "limit": 1}, "Search fixture 0"},
		{operatortool.WorkList, map[string]any{"fingerprint": "native-search-existing", "open": true, "limit": 1}, `"items":[]`},
		{operatortool.WorkList, map[string]any{"query": "native-search-existing", "label": "search-fixture", "open": false, "limit": 1}, `"items":[]`},
		{operatortool.WorkList, map[string]any{"query": "native-search-existing", "state": "Todo", "label": "search-fixture", "assignee": "operator", "priority": 0, "limit": 1}, "Search fixture"},
		{operatortool.WorkList, map[string]any{"query": "Search fixture", "states": []string{"In Review", "Todo"}, "labels": []string{"search-fixture", "absent"}, "assignees": []string{"operator", "absent"}, "priorities": []int{0, 3}, "archived": "all", "include": []string{"work", "workspace"}, "limit": 1}, `"lanes"`},
		{operatortool.WorkList, map[string]any{"archived": "true", "limit": 1}, selected.Title},
		{operatortool.WorkList, map[string]any{"query": "zzzzunfindable", "limit": 1}, `"items":[]`},
		{operatortool.WorkItem, map[string]any{"reference": issue.ID}, issue.Title},
		{operatortool.WorkComments, map[string]any{"reference": issue.ID, "limit": 1}, "Genuine native discussion"},
		{operatortool.WorkHistory, map[string]any{"reference": issue.ID, "limit": 1}, "next_cursor"},
		{operatortool.ListChanges, map[string]any{"work_item_id": issue.ID, "limit": 1}, change.ID},
		{operatortool.GetChange, map[string]any{"work_item_id": issue.ID, "change_id": change.ID}, version.HeadSHA},
		{operatortool.WorkRuns, map[string]any{"reference": issue.ID, "limit": 1}, owner.data.AttemptID},
		{operatortool.WorkRuns, map[string]any{"reference": issue.ID, "limit": 20}, owner.data.AttemptID},
		{operatortool.WorkRuns, map[string]any{"reference": issue.ID, "limit": 200}, owner.data.AttemptID},
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
			if test.name == operatortool.WorkRuns {
				var runs operatortool.WorkReadResult[tracker.Page[tracker.NativeAttempt]]
				if err := json.Unmarshal([]byte(result.Content), &runs); err != nil {
					t.Fatal(err)
				}
				if len(runs.Data.Items) != 1 || runs.Data.Items[0].Runtime != nil {
					t.Fatal("run summary included runtime detail")
				}
			}
			if test.name == operatortool.WorkAttemptReceipt {
				var receipt operatortool.WorkReadResult[tracker.NativeRuntimeEvidence]
				if err := json.Unmarshal([]byte(result.Content), &receipt); err != nil {
					t.Fatal(err)
				}
				attempt := receipt.Data.Attempt
				if receipt.GeneratedAt.IsZero() || receipt.GeneratedAt != receipt.Data.ObservedAt || attempt == nil || attempt.AttemptID != owner.data.AttemptID || attempt.RuntimeFreshness != "available" || attempt.Runtime == nil || len(attempt.Runtime.Phases) != 2 || attempt.Runtime.Phases[0].FinishedAt.IsZero() || attempt.Runtime.Activity == nil || len(attempt.Runtime.Activity.Spans) != 0 || attempt.Runtime.Validation == nil || attempt.Runtime.Validation.Output != strings.Repeat("x", 64<<10) {
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
		{operatortool.WorkItem, map[string]any{"project_id": foreignProject.ID, "reference": foreignIssue.WorkItemID, "view": "recovery"}, operatortool.ErrAccessDenied},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": foreignIssue.WorkItemID, "view": "recovery"}, operatortool.ErrAccessDenied},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": issue.ID, "view": "unsupported"}, operatortool.ErrInvalidArguments},
		{operatortool.WorkItem, map[string]any{"project_id": h.project, "reference": issue.ID, "cursor": "unsupported"}, operatortool.ErrInvalidArguments},
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
	finished := owner.data
	finished.Sequence++
	finished.Outcome = "failed"
	finished.CompletionBody = strings.Repeat("<", 64<<10)
	if err := h.native.AppendEvent(t.Context(), item, tracker.NativeRunEvent{Mutation: nativeMutationKey(), Type: "run.finished", SchemaVersion: 1, Data: finished}); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 20, 200} {
		t.Run("oversized_run_summary/"+strconv.Itoa(limit), func(t *testing.T) {
			arguments, err := json.Marshal(map[string]any{"project_id": string(h.project), "reference": issue.ID, "limit": limit})
			if err != nil {
				t.Fatal(err)
			}
			result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkRuns, Arguments: arguments})
			var refusal operatortool.RequestError
			if err == nil || result.Success || json.Unmarshal([]byte(result.Content), &refusal) != nil || refusal.Code != "attempt_record_too_large" || !strings.Contains(refusal.Message, "65536-byte") {
				t.Fatalf("lost run page bound: result=%s error=%v", result.Content, err)
			}
		})
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
	retainedArgs, err := json.Marshal(map[string]string{"project_id": string(h.project), "reference": issue.ID, "view": "recovery"})
	if err != nil {
		t.Fatal(err)
	}
	result, err = handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkItem, Arguments: retainedArgs})
	if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || result.Success {
		t.Fatal("retained read bypassed execution authority")
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

func TestNativeChangeOversizedReviewRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	h := newNativeChangeHub(t, true)
	type evidence struct {
		name    string
		item    tracker.NativeWorkItemID
		change  tracker.ChangeRequest
		version tracker.ChangeVersion
		reviews []tracker.ChangeReview
	}
	var fixtures []evidence
	for i, field := range []string{"criterion", "security finding", "implementation commands", "second implementation commands"} {
		issue := h.createInProgress(t, "Oversized retained "+field)
		item := tracker.NativeWorkItemID(issue.ID)
		change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Retained review"})
		if err != nil {
			t.Fatal(err)
		}
		version := h.publish(t, item, change.ID, nativeChangeFixtureHead([]string{"b", "c", "d", "e"}[i]))
		reviews := []tracker.ChangeReview{
			{ID: fmt.Sprintf("review_small_%d", i), VersionID: version.ID, Decision: "approved", Body: "Initial review"},
			{ID: fmt.Sprintf("review_large_%d", i), VersionID: version.ID, Decision: "changes_requested"},
		}
		large := &reviews[1]
		text := strings.Repeat("<\\\"\n世界", 40000)
		switch i {
		case 0:
			large.Validator = &gate.ValidatorResult{VersionID: version.ID, HeadSHA: version.HeadSHA, Verdict: "rework", Submitted: true, Findings: []gate.Finding{{Severity: "p1", Body: "Failed criterion"}}, CriteriaEvidence: []gate.CriterionEvidence{{Criterion: "Retained criterion", Kind: gate.EvidenceNotVerified, Behavior: text, ReceiptIDs: []string{"receipt_failed"}, HeadSHA: version.HeadSHA}}, Commands: []gate.CommandResult{{ReceiptID: "receipt_failed", HeadSHA: version.HeadSHA, ExitCode: 1, Output: "Failed"}}}
		case 1:
			large.SecurityAudit = &tracker.SecurityAuditReview{VersionID: version.ID, Verdict: "rework", Run: securityaudit.Run{Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Verdict: "fail", Findings: []securityaudit.Finding{{ID: "finding_blocking", Severity: "p2", Body: text}}}}
		default:
			result := &gate.ValidatorResult{VersionID: version.ID, Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Verdict: "rework", Submitted: true, Findings: []gate.Finding{{Severity: "p2", Body: "Command evidence requires rework"}}}
			for command := range 76 {
				receipt := fmt.Sprintf("receipt_%d_%02d", i, command)
				result.Commands = append(result.Commands, gate.CommandResult{ReceiptID: receipt, Execution: "executed", Command: fmt.Sprintf("go test -timeout=60s ./internal/example%d -run TestCase%d", i, command), HeadSHA: version.HeadSHA, TreeSHA: strings.Repeat("f", 40), ExitCode: command % 2, Output: fmt.Sprintf("Command %d result\n", command) + strings.Repeat("output <\\\"\n世界", 500)})
				result.CriteriaEvidence = append(result.CriteriaEvidence, gate.CriterionEvidence{Criterion: fmt.Sprintf("Command criterion %d", command), Kind: gate.EvidenceNotVerified, Behavior: "Command evidence retained", ReceiptIDs: []string{receipt}, HeadSHA: version.HeadSHA})
			}
			large.ImplementationReview = result
			bounded := *result
			bounded.Commands = slices.Clone(result.Commands)
			for command := range bounded.Commands {
				bounded.Commands[command] = tracker.PublicGateResult(bounded.Commands[command])
				if !bounded.Commands[command].OutputTruncated {
					t.Fatal("many-command fixture did not exercise normal output bounding")
				}
			}
			raw, err := json.Marshal(bounded)
			if err != nil || len(raw) <= operatortool.MaxResultBytes {
				t.Fatalf("many-command review must overflow after normal output bounding: bytes=%d error=%v", len(raw), err)
			}
			t.Logf("%s: commands=%d bounded_review_bytes=%d", field, len(result.Commands), len(raw))
		}
		fixtures = append(fixtures, evidence{name: field, item: item, change: change, version: version, reviews: reviews})
	}
	h.reopen(func(path string) error {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return err
		}
		defer db.Close()
		for _, fixture := range fixtures {
			for _, review := range fixture.reviews {
				raw, err := json.Marshal(review)
				if err != nil {
					return err
				}
				if _, err := db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, source_key, record_json) VALUES (?, ?, 'review', ?, ?)", fixture.change.ID, review.VersionID, review.ID, string(raw)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	nativeTransport := h.native.client.httpClient.Transport
	h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		response, err := nativeTransport.RoundTrip(request)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(data) > operatortool.MaxResultBytes {
			t.Errorf("recovery transport exceeded byte cap: %d", len(data))
		}
		response.Body = io.NopCloser(strings.NewReader(string(data)))
		return response, nil
	})
	t.Cleanup(func() { h.native.client.httpClient.Transport = nativeTransport })
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			path, err := changePath(fixture.item, fixture.change.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			var first operatortool.ChangeResult
			if err := h.native.client.request(t.Context(), http.MethodGet, h.native.base()+path+"?view=bounded", nil, &first); err != nil {
				t.Fatal(err)
			}
			if first.Read.Sections["reviews"].Complete || len(first.Detail.Reviews) != 1 {
				t.Fatal("fixture must fit its current page and defer its oversized review")
			}
			var next operatortool.ChangeResult
			err = h.native.client.request(t.Context(), http.MethodGet, h.native.base()+path+"?view=bounded&section=reviews&cursor="+first.Read.Sections["reviews"].NextCursor, nil, &next)
			if err != nil || next.Fragment == nil || next.Read.Sections["reviews"].Complete || len(next.Detail.Reviews) != 0 {
				t.Fatalf("oversized continuation did not return a bounded fragment: %v", err)
			}
			newer, err := h.admin.CreateChange(t.Context(), fixture.item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Newer independent Change"})
			if err != nil {
				t.Fatal(err)
			}
			newerVersion := h.publish(t, fixture.item, newer.ID, nativeChangeFixtureHead("f"))
			latest, err := h.native.currentChange(t.Context(), fixture.item)
			if err != nil || latest.Change.ID != newer.ID || latest.Change.CurrentVersion != newerVersion.ID {
				t.Fatalf("newer Change is not independently usable: %v", err)
			}
			detail, err := h.native.runtimeChange(t.Context(), fixture.item, fixture.change.ID)
			if err != nil {
				t.Fatalf("recover oversized retained review: %v", err)
			}
			if !reflect.DeepEqual(detail.Reviews, fixture.reviews) {
				t.Fatal("recovery lost complete review verdicts, criteria, findings or receipts")
			}
			if detail.Change.ID != fixture.change.ID || detail.Change.CurrentVersion != fixture.version.ID || len(detail.Versions) != 1 || detail.Versions[0].ID != fixture.version.ID || detail.Versions[0].HeadSHA != fixture.version.HeadSHA || !reflect.DeepEqual(detail.Versions[0].Source, fixture.version.Source) {
				t.Fatal("recovery changed immutable source/version identity")
			}
			state, err := h.connector.ReadChangeReview(t.Context(), string(fixture.item), fixture.change.ID, fixture.version.ID, fixture.version.HeadSHA)
			if err != nil || !state.Current || state.Reviewed || !state.ChangesRequested {
				t.Fatalf("controller recovery changed failing review state: %+v, %v", state, err)
			}
			canonical, err := h.admin.Change(t.Context(), fixture.item, fixture.change.ID)
			if err != nil || !reflect.DeepEqual(canonical.Reviews, fixture.reviews) {
				t.Fatalf("recovery changed canonical reviews: %v", err)
			}
		})
	}
}

func TestNativeExecutionRetainedRecoveryEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	h := newNativeChangeHub(t, true)
	cfg := config.Default()
	cfg.Tracker.Kind = config.TrackerHubNative
	descriptor, err := config.ResolvePolicy(config.Workflow{Config: cfg, Prompt: strings.Repeat("p", 320<<10)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: descriptor}); err != nil {
		t.Fatal(err)
	}
	h.descriptor = descriptor
	issue := h.createInProgress(t, "Retained evidence worker")
	body := issueContractTestSections + strings.Repeat("<", 153<<10) + strings.Repeat("b", (256<<10)-len(issueContractTestSections)-(153<<10))
	currentIssue, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.UpdateIssue(t.Context(), currentIssue.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, Body: &body}); err != nil {
		t.Fatal(err)
	}
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID)
	if execution == nil {
		t.Fatal("claimed worker execution is missing")
	}
	if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "rework", Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	tools, handler := execution.(runner.ToolExecution).AgentTools()
	var schema json.RawMessage
	for _, tool := range tools {
		if tool.Name == operatortool.WorkItem {
			schema = tool.InputSchema
		}
	}

	items := []tracker.NativeWorkItemID{tracker.NativeWorkItemID(issue.ID)}
	other := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Second retained item", Body: body, State: "Todo"})
	items = append(items, other.WorkItemID)
	type evidence struct {
		change   tracker.ChangeRequest
		versions []tracker.ChangeVersion
		reviews  []tracker.ChangeReview
		comment  tracker.NativeComment
		landing  *tracker.SecurityAuditReview
	}
	wants := make(map[tracker.NativeWorkItemID]evidence)
	for i, item := range items {
		change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: fmt.Sprintf("Retained change %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		old := h.publish(t, item, change.ID, nativeChangeFixtureHead("b"))
		current := h.publish(t, item, change.ID, nativeChangeFixtureHead("c"), old.ID)
		comment, err := h.admin.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: nativeMutationKey(), Body: strings.Repeat("retained discussion <\n", 2800)})
		if err != nil {
			t.Fatal(err)
		}
		reviews := []tracker.ChangeReview{
			{ID: fmt.Sprintf("review_human_%d", i), VersionID: current.ID, Decision: "changes_requested", Body: strings.Repeat("human feedback <\n", 65536), Actor: tracker.Actor{Kind: "human", PrincipalID: "reviewer"}, CreatedAt: current.CreatedAt},
			{ID: fmt.Sprintf("review_commands_%d", i), VersionID: old.ID, Decision: "approved", Actor: tracker.Actor{Kind: "runner", PrincipalID: "worker"}, CreatedAt: old.CreatedAt, ImplementationReview: &gate.ValidatorResult{VersionID: old.ID, Repository: old.Repository, BaseSHA: old.BaseSHA, HeadSHA: old.HeadSHA, Submitted: true, Verdict: "pass", Commands: []gate.CommandResult{{Command: "make check-land", Output: strings.Repeat("command output <\n", 40000)}}}},
		}
		now := time.Now().UTC()
		run := securityaudit.Run{ID: int64(42 + i), InvocationID: fmt.Sprintf("audit-%d", i), ProjectID: string(h.project), IssueID: string(item), Repository: current.Repository, BaseSHA: current.BaseSHA, HeadSHA: current.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now, CompletedAt: now, ExitStatus: securityaudit.ExitStatusSuccess, OutputBytes: 1, OutputDigest: policy.Digest([]byte("audit")), Verdict: "pass", Summary: strings.Repeat("s", (1<<20)-(16<<10))}
		audit := &tracker.SecurityAuditReview{VersionID: current.ID, SessionID: run.ID, Verdict: "pass", DiffDigest: policy.Digest([]byte("diff")), Run: run}
		decision := "approved"
		if i == 1 {
			audit.Run.Summary = "Blocking audit"
			audit.Run.Verdict, audit.Verdict, decision = "fail", "rework", "changes_requested"
			audit.Run.Findings = []securityaudit.Finding{{ID: "blocking", Severity: "p2", Body: strings.Repeat("finding <\n", 90_000)}, {ID: "resolved", Severity: "p1", Status: "resolved", Body: "Already addressed"}}
		}
		reviews = append(reviews, tracker.ChangeReview{ID: fmt.Sprintf("review_audit_%d", i), VersionID: current.ID, Decision: decision, SecurityAudit: audit, Actor: tracker.Actor{Kind: "runner", PrincipalID: "audit-worker"}, CreatedAt: current.CreatedAt})
		var landing *tracker.SecurityAuditReview
		if i == 0 {
			retained := *audit
			retained.Landing = true
			retained.Run.Summary = "All findings resolved"
			for finding := range 6000 {
				retained.Run.Findings = append(retained.Run.Findings, securityaudit.Finding{ID: fmt.Sprintf("finding-%06d-%s", finding, strings.Repeat("i", 64)), Status: "resolved", Severity: "p1", Body: "Resolved"})
			}
			landing = &retained
			observation := tracker.NativeRuntimeObservation{Phase: "merging", Landing: &tracker.NativeLandingReceipt{SecurityAudit: landing, ChangeID: change.ID, VersionID: current.ID, HeadSHA: current.HeadSHA, GateFailed: true, ObservedAt: now}}
			if err := execution.(runner.RuntimeExecution).ObserveRuntime(t.Context(), observation); err != nil {
				t.Fatal(err)
			}
		}
		wants[item] = evidence{change: change, versions: []tracker.ChangeVersion{old, current}, reviews: reviews, comment: comment, landing: landing}
	}
	h.reopen(func(path string) error {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return err
		}
		defer db.Close()
		for _, want := range wants {
			for _, review := range want.reviews {
				raw, err := json.Marshal(review)
				if err != nil {
					return err
				}
				if _, err := db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, source_key, record_json) VALUES (?, ?, 'review', ?, ?)", want.change.ID, review.VersionID, review.ID, string(raw)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	for _, item := range items {
		t.Run(string(item), func(t *testing.T) {
			want := wants[item]
			recovery, err := h.native.Recovery(t.Context(), item)
			if err != nil {
				t.Fatalf("large audit prevented worker recovery: %v", err)
			}
			if recovery.Issue.Body != body || recovery.Read == nil || recovery.Read.IssueBody == nil {
				t.Fatal("combined evidence lost required instructions before worker dispatch")
			}
			if recovery.ChangeDetail == nil || recovery.Read == nil || !slices.Contains(recovery.Read.Omitted, "change_detail.reviews") {
				t.Fatal("projected audit did not expose retained evidence")
			}
			if want.landing != nil {
				copies := []*tracker.NativeLandingReceipt{recovery.ChangeDetail.Versions[0].Landing}
				for _, attempt := range recovery.Attempts {
					if attempt.Runtime != nil && attempt.Runtime.Landing != nil {
						copies = append(copies, attempt.Runtime.Landing)
					}
				}
				if len(copies) != 2 {
					t.Fatal("worker lost a landing copy")
				}
				for _, receipt := range copies {
					if receipt == nil || receipt.SecurityAudit == nil || !receipt.SecurityAudit.RunOmitted || receipt.SecurityAudit.SessionID != want.landing.SessionID || receipt.SecurityAudit.Verdict != want.landing.Verdict || !receipt.GateFailed {
						t.Fatal("worker lost bounded landing identity or gate result")
					}
				}
			}
			original := want.reviews[len(want.reviews)-1]
			var projected tracker.ChangeReview
			for _, review := range recovery.ChangeDetail.Reviews {
				if review.ID == original.ID {
					projected = review
				}
			}
			if projected.SecurityAudit == nil || !projected.SecurityAudit.TextTruncated || projected.Decision != original.Decision || projected.VersionID != original.VersionID {
				t.Fatal("bounded recovery lost authoritative audit identity or decision")
			}
			got, expected := *projected.SecurityAudit, *original.SecurityAudit
			if len(got.Run.Summary) > 4096 || strings.TrimSpace(got.Run.Summary) == "" || len(got.Run.Findings) != len(expected.Run.Findings) {
				t.Fatal("audit prose remained unbounded or lost finding state")
			}
			key := securityaudit.Key{ProjectID: string(h.project), Repository: expected.Run.Repository, BaseSHA: expected.Run.BaseSHA, HeadSHA: expected.Run.HeadSHA}
			for _, severities := range [][]string{{"p1", "p2"}, {"p3"}, {"none"}} {
				actual := securityaudit.Evaluate(got.Run, nil, key, "", severities)
				before := securityaudit.Evaluate(expected.Run, nil, key, "", severities)
				if actual.Allowed != before.Allowed || actual.Reason != before.Reason || len(actual.Findings) != len(before.Findings) {
					t.Fatal("projection changed audit validity or configured blocking semantics")
				}
			}
			got.TextTruncated = false
			got.Run.Summary = expected.Run.Summary
			got.Run.Findings = slices.Clone(got.Run.Findings)
			for i := range got.Run.Findings {
				if len(got.Run.Findings[i].Body) > 4096 || len(got.Run.Findings[i].Path) > 4096 {
					t.Fatal("unbounded finding prose")
				}
				got.Run.Findings[i].Body, got.Run.Findings[i].Path = expected.Run.Findings[i].Body, expected.Run.Findings[i].Path
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatal("projection changed audit identity, verdict, timestamps, digest or finding ID/status/severity")
			}
			seen := map[string]bool{}
			cursor := ""
			var record []byte
			var identity tracker.NativeRecoveryPage
			for pages := 0; ; pages++ {
				if pages > 200 {
					t.Fatal("retained traversal did not terminate")
				}
				args := map[string]any{"project_id": h.project, "reference": item, "view": "recovery"}
				if cursor != "" {
					args["cursor"] = cursor
				}
				raw, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkItem, Arguments: raw})
				var response operatortool.WorkReadResult[tracker.NativeRecoveryPage]
				if err != nil || !result.Success || len(result.Content) > operatortool.MaxResultBytes || json.Unmarshal([]byte(result.Content), &response) != nil {
					t.Fatalf("worker retained read: success=%t bytes=%d err=%v", result.Success, len(result.Content), err)
				}
				if !strings.Contains(string(schema), "view") {
					t.Fatal("worker work_item schema cannot request retained recovery evidence")
				}
				page := response.Data
				if page.WorkItemID != item || page.ProjectID != h.project || page.OrganizationID != h.organization {
					t.Fatal("fragment lost scoped identity")
				}
				if page.Section == "complete" {
					if len(record) != 0 {
						t.Fatal("truncated final record")
					}
					break
				}
				if page.Offset != int64(len(record)) || len(page.Data) == 0 || len(page.Data) > 64<<10 || page.NextCursor == "" || page.NextCursor == cursor {
					t.Fatal("unbounded or non-progressing fragment")
				}
				if page.Offset == 0 {
					identity = page
				} else if page.Section != identity.Section || page.RecordID != identity.RecordID || page.VersionID != identity.VersionID || page.TotalBytes != identity.TotalBytes || page.SHA256 != identity.SHA256 {
					t.Fatal("fragment identity changed")
				}
				if cursor == "" {
					foreignItem := items[0]
					if item == foreignItem {
						foreignItem = items[1]
					}
					foreignArgs, err := json.Marshal(map[string]any{"project_id": h.project, "reference": foreignItem, "view": "recovery", "cursor": page.NextCursor})
					if err != nil {
						t.Fatal(err)
					}
					refusal, err := handler(t.Context(), runner.AgentToolCall{Name: operatortool.WorkItem, Arguments: foreignArgs})
					if err == nil || refusal.Success {
						t.Fatal("worker accepted another item's cursor")
					}
				}
				record = append(record, page.Data...)
				if int64(len(record)) == page.TotalBytes {
					digest := sha256.Sum256(record)
					if hex.EncodeToString(digest[:]) != page.SHA256 {
						t.Fatal("retained record digest changed")
					}
					switch page.Section {
					case "reviews":
						var got tracker.ChangeReview
						if err := json.Unmarshal(record, &got); err != nil {
							t.Fatal(err)
						}
						for _, review := range want.reviews {
							if review.ID == got.ID {
								if page.VersionID != review.VersionID || !reflect.DeepEqual(got, review) {
									t.Fatal("worker lost complete ordinary review or command output")
								}
								seen[review.ID] = true
							}
						}
					case "versions":
						var got tracker.ChangeVersion
						if err := json.Unmarshal(record, &got); err != nil {
							t.Fatal(err)
						}
						for _, version := range want.versions {
							if version.ID == got.ID {
								if page.VersionID != version.ID || !reflect.DeepEqual(got.Policy, version.Policy) || got.HeadSHA != version.HeadSHA || !reflect.DeepEqual(got.Source, version.Source) {
									t.Fatal("worker lost immutable source or historical frozen policy")
								}
								if want.landing != nil && version.ID == want.versions[len(want.versions)-1].ID && (got.Landing == nil || !reflect.DeepEqual(got.Landing.SecurityAudit, want.landing)) {
									t.Fatal("worker lost exact version landing audit")
								}
								seen[version.ID] = true
							}
						}
					case "attempts":
						var got tracker.NativeAttempt
						if err := json.Unmarshal(record, &got); err != nil {
							t.Fatal(err)
						}
						if want.landing != nil && got.Runtime != nil && got.Runtime.Landing != nil {
							if !reflect.DeepEqual(got.Runtime.Landing.SecurityAudit, want.landing) || !got.Runtime.Landing.GateFailed {
								t.Fatal("worker lost exact attempt landing audit")
							}
							seen[page.RecordID] = true
						}
					case "discussion":
						var got tracker.NativeComment
						if err := json.Unmarshal(record, &got); err != nil {
							t.Fatal(err)
						}
						if got.ID == want.comment.ID {
							if !reflect.DeepEqual(got, want.comment) {
								t.Fatal("worker lost retained discussion")
							}
							seen[got.ID] = true
						}
					}
					record = nil
				}
				cursor = page.NextCursor
			}
			expectedRecords := 6
			if want.landing != nil {
				expectedRecords++
			}
			if len(seen) != expectedRecords {
				t.Fatalf("worker lost retained evidence: %v", seen)
			}
		})
	}
}
