package web_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/activity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	web "github.com/digitaldrywood/detent/internal/web"
)

func TestAPIBoardCardRendersLiveActivityAndVerboseUsage(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	issue := telemetry.Issue{ID: "issue-1156", Identifier: "digitaldrywood/detent#1156", ProjectID: "detent", Title: "Live timeline", State: "In Progress"}
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: at,
		BoardIssues: []telemetry.Issue{issue},
		SchedulerDecisions: []telemetry.SchedulerDecision{{
			ID: 1, ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, Result: "skipped", Reason: "artifact_gate_wait_status", DecisionAt: at,
		}},
		Running: []telemetry.Running{{
			Issue:        issue,
			RecentEvents: []telemetry.ActivityEvent{{At: at.Add(time.Second), Event: "token_usage", Message: "125 total tokens"}},
			Tokens:       telemetry.Tokens{Total: 125},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/card?project=detent&issue=issue-1156", http.StatusOK)
	for _, want := range []string{"Loading orchestration activity", `aria-busy="true"`, "Live session", "data-board-live-session", "flex min-h-72 flex-none flex-col", "hx-preserve"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail sheet missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "125 total tokens") {
		t.Fatalf("default activity unexpectedly includes verbose usage:\n%s", body)
	}

	body = requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/activity?project=detent&issue=issue-1156&verbose=1", http.StatusOK)
	for _, want := range []string{"Dispatch skipped", "artifact_gate_wait_status", "125 total tokens", "Hide usage ticks", "data-activity-list-scroll", "max-h-[24rem] overflow-y-auto overscroll-contain"} {
		if !strings.Contains(body, want) {
			t.Fatalf("verbose activity missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "125 total tokens") || !strings.Contains(body, "Hide usage ticks") {
		t.Fatalf("verbose activity missing token usage:\n%s", body)
	}
}

func TestAPIBoardCardRendersAuthorizationSelectorDetail(t *testing.T) {
	t.Parallel()

	const detail = "issue does not match authorization selector: missing required label `detent`"
	at := time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC)
	issue := telemetry.Issue{ID: "issue-532", Identifier: "gopherguides/corp#532", ProjectID: "corp", Title: "Hand-filed issue", State: "Todo"}
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: at,
		BoardIssues: []telemetry.Issue{issue},
		SchedulerDecisions: []telemetry.SchedulerDecision{{
			ID: 1, ProjectID: "corp", IssueID: issue.ID, Identifier: issue.Identifier, Result: "skipped", Reason: "authorization_selector_declined", WaitReason: detail, DecisionAt: at,
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/activity?project=corp&issue=issue-532", http.StatusOK)
	for _, want := range []string{"Dispatch skipped", detail} {
		if !strings.Contains(body, want) {
			t.Fatalf("board activity missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, ">unauthorized<") {
		t.Fatalf("board activity presents selector decline as credential failure:\n%s", body)
	}
}

func TestBoardLiveSessionPageRendersActiveSessionFullWidth(t *testing.T) {
	t.Parallel()

	issue := telemetry.Issue{ID: "issue-1239", Identifier: "digitaldrywood/detent#1239", ProjectID: "detent", Title: "Readable live session", State: "In Progress"}
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{issue},
		Running:     []telemetry.Running{{Issue: issue, DetentSessionID: 1239, SessionID: "thread-1239"}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := requestHTML(t, server.Handler(), http.MethodGet, "/live-session?project=detent&issue=issue-1239&identifier=digitaldrywood%2Fdetent%231239", http.StatusOK)
	for _, want := range []string{
		"<!doctype html>",
		"data-live-session-page",
		"data-board-live-session",
		"Attached read-only",
		"flex min-h-0 flex-1 flex-col bg-page text-left",
		`sse-connect="/api/v1/board/session/events?display=full&amp;identifier=digitaldrywood%2Fdetent%231239&amp;issue=issue-1239&amp;project=detent"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("full-page live session missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Open full-page view") {
		t.Fatalf("full-page live session contains recursive pop-out link:\n%s", body)
	}

	fragment := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/session?project=detent&issue=issue-1239&identifier=digitaldrywood%2Fdetent%231239", http.StatusOK)
	for _, want := range []string{
		"Open full-page view",
		`target="_blank"`,
		`href="/live-session?identifier=digitaldrywood%2Fdetent%231239&amp;issue=issue-1239&amp;project=detent"`,
	} {
		if !strings.Contains(fragment, want) {
			t.Fatalf("sheet live session missing %q:\n%s", want, fragment)
		}
	}
}

func TestAPIBoardCardKeysPreservedSessionByIssue(t *testing.T) {
	t.Parallel()

	issues := []telemetry.Issue{
		{ID: "issue-1156", Identifier: "digitaldrywood/detent#1156", ProjectID: "detent", Title: "First card", State: "In Progress"},
		{ID: "issue-1157", Identifier: "digitaldrywood/detent#1157", ProjectID: "detent", Title: "Second card", State: "In Progress"},
	}
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{BoardIssues: issues}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	firstBody := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/card?project=detent&issue=issue-1156", http.StatusOK)
	secondBody := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/card?project=detent&issue=issue-1157", http.StatusOK)
	firstID := liveSessionElementID(t, firstBody)
	secondID := liveSessionElementID(t, secondBody)
	if firstID == secondID {
		t.Fatalf("live session id = %q for both issues, want issue-keyed preservation", firstID)
	}
	for _, rendered := range []struct {
		body string
		id   string
	}{{body: firstBody, id: firstID}, {body: secondBody, id: secondID}} {
		if !strings.Contains(rendered.body, `hx-target="#`+rendered.id+`"`) {
			t.Fatalf("session target missing keyed id %q:\n%s", rendered.id, rendered.body)
		}
	}
}

func TestAPIBoardCardRotatesPreservedSessionWithWorkerLifecycle(t *testing.T) {
	t.Parallel()

	issue := telemetry.Issue{ID: "issue-1156", Identifier: "digitaldrywood/detent#1156", ProjectID: "detent", Title: "Lifecycle card", State: "In Progress"}
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{issue},
		Running:     []telemetry.Running{{Issue: issue, DetentSessionID: 42, SessionID: "thread-42"}},
	}); err != nil {
		t.Fatalf("Publish(active) error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	activeBody := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/card?project=detent&issue=issue-1156", http.StatusOK)
	if err := deps.Hub.Publish(telemetry.Snapshot{BoardIssues: []telemetry.Issue{issue}}); err != nil {
		t.Fatalf("Publish(inactive) error = %v", err)
	}
	inactiveBody := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/card?project=detent&issue=issue-1156", http.StatusOK)
	activeID := liveSessionElementID(t, activeBody)
	inactiveID := liveSessionElementID(t, inactiveBody)
	if activeID == inactiveID {
		t.Fatalf("live session id = %q before and after worker stop, want lifecycle-keyed replacement", activeID)
	}
}

func TestAPIBoardSessionAttachBackfillsAndFollows(t *testing.T) {
	t.Parallel()

	issue := telemetry.Issue{ID: "issue-1156", Identifier: "digitaldrywood/detent#1156", ProjectID: "detent", Title: "Live transcript", State: "In Progress"}
	broker := activity.NewBroker()
	deps := testDeps(t)
	deps.Activity = broker
	if err := deps.Hub.Publish(telemetry.Snapshot{BoardIssues: []telemetry.Issue{issue}, Running: []telemetry.Running{{Issue: issue}}}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	broker.Publish(activity.Key{ProjectID: "detent", IssueID: issue.ID}, activity.Event{DetentSessionID: 7, Kind: "tool_output", Title: "Tool output · exec", Content: "backfill output"})
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/board/session/events?project=detent&issue=issue-1156", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	reader := bufio.NewReader(response.Body)
	if data := readBoardSSEData(t, reader); !strings.Contains(data, "backfill output") {
		t.Fatalf("backfill SSE = %q", data)
	}
	if got := broker.SubscriberCount(activity.Key{ProjectID: "detent", IssueID: issue.ID}); got != 1 {
		t.Fatalf("SubscriberCount() = %d, want 1", got)
	}

	broker.Publish(activity.Key{ProjectID: "detent", IssueID: issue.ID}, activity.Event{DetentSessionID: 7, Kind: "assistant", Title: "Agent", Content: "live output"})
	if data := readBoardSSEData(t, reader); !strings.Contains(data, "live output") {
		t.Fatalf("live SSE = %q", data)
	}
	cancel()
	_ = response.Body.Close()
	waitForBoardSubscriberCount(t, broker, activity.Key{ProjectID: "detent", IssueID: issue.ID}, 0)
}

func TestAPIBoardActivityStreamShowsDispatchSkipWithinTick(t *testing.T) {
	t.Parallel()

	issue := telemetry.Issue{ID: "issue-1156", Identifier: "digitaldrywood/detent#1156", ProjectID: "detent", Title: "Live skip", State: "In Progress"}
	backend := openWebTestStore(t)
	deps := testDeps(t)
	deps.Store = backend
	if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: time.Now().UTC(), BoardIssues: []telemetry.Issue{issue}}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir(), SSETickInterval: 10 * time.Millisecond}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/board/activity/events?project=detent&issue=issue-1156", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	reader := bufio.NewReader(response.Body)
	_ = readBoardSSEData(t, reader)

	if _, err := backend.RecordSchedulerDecision(ctx, store.SchedulerDecision{
		ProjectID:  "detent",
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Result:     store.SchedulerDecisionResultSkipped,
		Reason:     "artifact_gate_wait_status",
		DecisionAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordSchedulerDecision() error = %v", err)
	}
	if data := readBoardSSEData(t, reader); !strings.Contains(data, "artifact_gate_wait_status") || !strings.Contains(data, "Dispatch skipped") {
		t.Fatalf("activity SSE = %q", data)
	}
}

func TestAPIBoardSessionPagesFailedRolloutHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	issue := telemetry.Issue{ID: "issue-1156", Identifier: "digitaldrywood/detent#1156", ProjectID: "detent", URL: "https://github.com/digitaldrywood/detent/issues/1156", Title: "Closed transcript", State: "Done"}
	backend := openWebTestStore(t)
	sessionID, err := backend.StartSession(ctx, store.SessionStart{
		ProjectID:        issue.ProjectID,
		IssueID:          issue.ID,
		Identifier:       issue.Identifier,
		IssueURL:         issue.URL,
		StartedAt:        at,
		Model:            "gpt-5.6-codex",
		RequestedModel:   "gpt-5.6-codex",
		AgentBackendID:   "codex-default",
		AgentBackendKind: "codex",
		AgentRole:        "code",
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if err := backend.FinishSession(ctx, sessionID, store.SessionFinish{
		CompletedAt:      at.Add(time.Minute),
		FinalState:       "failed",
		ProviderThreadID: "thread-1156",
	}); err != nil {
		t.Fatalf("FinishSession() error = %v", err)
	}

	deps := testDeps(t)
	deps.Store = backend
	deps.History = fixedHistoryReader{page: activity.HistoryPage{
		Events:  []activity.Event{{At: at, Kind: "assistant", Title: "Agent", Content: "rollout output"}},
		Limit:   50,
		HasMore: true,
	}}
	if err := deps.Hub.Publish(telemetry.Snapshot{BoardIssues: []telemetry.Issue{issue}}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	sheetBody := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/card?project=detent&issue=issue-1156", http.StatusOK)
	mountedSessionID := liveSessionElementID(t, sheetBody)
	body := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/session?project=detent&issue=issue-1156", http.StatusOK)
	if !strings.Contains(body, "View rollout history") {
		t.Fatalf("closed session missing rollout action:\n%s", body)
	}
	if !strings.Contains(body, `hx-target="#`+mountedSessionID+`"`) {
		t.Fatalf("closed session rollout target does not match mounted host %q:\n%s", mountedSessionID, body)
	}
	body = requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/session/history?project=detent&issue=issue-1156&limit=50&display=full", http.StatusOK)
	for _, want := range []string{"Provider rollout history", "rollout output", "Load older rollout events", "min-w-max whitespace-pre text-left", "display=full"} {
		if !strings.Contains(body, want) {
			t.Fatalf("rollout history missing %q:\n%s", want, body)
		}
	}
}

type fixedHistoryReader struct {
	page activity.HistoryPage
}

func (r fixedHistoryReader) Page(context.Context, activity.HistoryQuery) (activity.HistoryPage, error) {
	return r.page, nil
}

func liveSessionElementID(t *testing.T, body string) string {
	t.Helper()
	const prefix = `id="board-live-session-`
	start := strings.Index(body, prefix)
	if start < 0 {
		t.Fatalf("live session element id missing:\n%s", body)
	}
	start += len(`id="`)
	end := strings.IndexByte(body[start:], '"')
	if end < 0 {
		t.Fatalf("live session element id unterminated:\n%s", body)
	}
	return body[start : start+end]
}

func readBoardSSEData(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString() error = %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return data.String()
		}
		if strings.HasPrefix(line, "data: ") {
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
}

func waitForBoardSubscriberCount(t *testing.T, broker *activity.Broker, key activity.Key, want int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	t.Cleanup(func() { deadline.Stop() })
	ticker := time.NewTicker(time.Millisecond)
	t.Cleanup(ticker.Stop)
	for {
		if broker.SubscriberCount(key) == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("SubscriberCount() = %d, want %d", broker.SubscriberCount(key), want)
		case <-ticker.C:
		}
	}
}

func TestAPIBoardCardRendersDetailSheet(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		BoardIssues: []telemetry.Issue{
			{
				ID:         "i1",
				Identifier: "digitaldrywood/detent#9510",
				ProjectID:  "demo-project",
				Title:      "Kanban demo backlog intake",
				State:      "Backlog",
				URL:        "https://github.com/digitaldrywood/detent/issues/9510",
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=demo-project&issue=digitaldrywood%2Fdetent%239510", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"data-detail-sheet",
		"Kanban demo backlog intake",
		"#9510",
		`href="https://github.com/digitaldrywood/detent/issues/9510"`,
		"data-sheet-close",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("sheet missing %q:\n%s", want, rec.Body.String())
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=demo-project&issue=9510", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy number status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=demo-project&issue=9999", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing card status = %d, want 404", rec.Code)
	}
}

func TestAPIBoardCardDoesNotWaitForCommentReaders(t *testing.T) {
	t.Parallel()

	issueStarted := make(chan struct{}, 1)
	issueRelease := make(chan struct{})
	prStarted := make(chan struct{}, 1)
	prRelease := make(chan struct{})
	connector := &blockingKanbanCommentConnector{
		kanbanActionConnector: &kanbanActionConnector{name: "github"},
		issueStarted:          issueStarted,
		issueRelease:          issueRelease,
		prStarted:             prStarted,
		prRelease:             prRelease,
	}
	deps := testDeps(t)
	storeStarted := make(chan string, 3)
	storeRelease := make(chan struct{})
	deps.Store = &blockingBoardDetailStore{Store: deps.Store, started: storeStarted, release: storeRelease}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, connector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		Projects: []telemetry.ProjectSnapshot{{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}}},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_async_comments",
			Identifier: "digitaldrywood/detent#1446",
			ProjectID:  "detent",
			Title:      "Open card details immediately",
			State:      "In Progress",
			PullRequest: &telemetry.PullRequest{
				Number: 1447,
				URL:    "https://github.com/digitaldrywood/detent/pull/1447",
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=I_async_comments&actions=board", nil)
		server.Handler().ServeHTTP(recorder, request)
		response <- recorder
	}()

	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case recorder := <-response:
		close(issueRelease)
		close(prRelease)
		close(storeRelease)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
		}
		for _, want := range []string{
			"Open card details immediately",
			"In Progress",
			"Loading efficiency receipt",
			"Loading orchestration activity",
			"Loading issue comments",
			`aria-busy="true"`,
			`/api/v1/board/receipt?`,
			`/api/v1/board/activity?`,
			`/api/v1/board/conversation?`,
		} {
			if !strings.Contains(recorder.Body.String(), want) {
				t.Fatalf("initial sheet missing %q:\n%s", want, recorder.Body.String())
			}
		}
	case <-timer.C:
		close(issueRelease)
		close(prRelease)
		close(storeRelease)
		<-response
		t.Fatal("initial detail sheet waited for a blocked enrichment")
	}

	select {
	case <-issueStarted:
		t.Fatal("initial detail sheet called FetchIssueComments")
	default:
	}
	select {
	case <-prStarted:
		t.Fatal("initial detail sheet called FetchPullRequestComments")
	default:
	}
	select {
	case operation := <-storeStarted:
		t.Fatalf("initial detail sheet called %s", operation)
	default:
	}
}

type blockingKanbanCommentConnector struct {
	*kanbanActionConnector
	issueStarted chan<- struct{}
	issueRelease <-chan struct{}
	prStarted    chan<- struct{}
	prRelease    <-chan struct{}
}

func (c *blockingKanbanCommentConnector) FetchIssueComments(ctx context.Context, _ connector.Issue) ([]connector.IssueComment, error) {
	select {
	case c.issueStarted <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.issueRelease:
		return nil, nil
	}
}

func (c *blockingKanbanCommentConnector) FetchPullRequestComments(ctx context.Context, _ string, _ int) ([]connector.IssueComment, error) {
	select {
	case c.prStarted <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.prRelease:
		return nil, nil
	}
}

type blockingBoardDetailStore struct {
	store.Store
	started chan<- string
	release <-chan struct{}
}

func (s *blockingBoardDetailStore) EfficiencyReceipt(ctx context.Context, _ string, _ string, _ string) (efficiency.Receipt, error) {
	if err := s.wait(ctx, "EfficiencyReceipt"); err != nil {
		return efficiency.Receipt{}, err
	}
	return efficiency.Receipt{}, store.ErrNotFound
}

func (s *blockingBoardDetailStore) ListIssueActivity(ctx context.Context, _ store.IssueActivityQuery) ([]store.IssueActivityEvent, error) {
	if err := s.wait(ctx, "ListIssueActivity"); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *blockingBoardDetailStore) LatestIssueAgentSession(ctx context.Context, _ store.IssueIdentity) (store.IssueAgentSession, error) {
	if err := s.wait(ctx, "LatestIssueAgentSession"); err != nil {
		return store.IssueAgentSession{}, err
	}
	return store.IssueAgentSession{}, store.ErrNotFound
}

func (s *blockingBoardDetailStore) wait(ctx context.Context, operation string) error {
	select {
	case s.started <- operation:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return nil
	}
}

func TestAPIBoardConversationHydratesCommentReadersIndependently(t *testing.T) {
	t.Parallel()

	issueStarted := make(chan struct{}, 1)
	issueRelease := make(chan struct{})
	prStarted := make(chan struct{}, 1)
	prRelease := make(chan struct{})
	connector := &blockingKanbanCommentConnector{
		kanbanActionConnector: &kanbanActionConnector{name: "github"},
		issueStarted:          issueStarted,
		issueRelease:          issueRelease,
		prStarted:             prStarted,
		prRelease:             prRelease,
	}
	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{Mode: workflowconfig.KanbanModeIntegration}, connector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		Projects: []telemetry.ProjectSnapshot{{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}}},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_split_comments",
			Identifier: "digitaldrywood/detent#1446",
			ProjectID:  "detent",
			Title:      "Hydrate comments independently",
			State:      "In Progress",
			PullRequest: &telemetry.PullRequest{
				Number: 1447,
				URL:    "https://github.com/digitaldrywood/detent/pull/1447",
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	issueResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/board/conversation?project=detent&issue=I_split_comments&actions=board&target=issue", nil)
		server.Handler().ServeHTTP(recorder, request)
		issueResponse <- recorder
	}()
	select {
	case <-issueStarted:
	case <-time.After(time.Second):
		t.Fatal("issue comment reader did not start")
	}
	select {
	case <-prStarted:
		t.Fatal("issue comment fragment called the PR comment reader")
	default:
	}
	close(issueRelease)
	if recorder := <-issueResponse; recorder.Code != http.StatusOK {
		t.Fatalf("issue status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	prResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/board/conversation?project=detent&issue=I_split_comments&actions=board&target=pr", nil)
		server.Handler().ServeHTTP(recorder, request)
		prResponse <- recorder
	}()
	select {
	case <-prStarted:
	case <-time.After(time.Second):
		t.Fatal("PR comment reader did not start")
	}
	select {
	case <-issueStarted:
		t.Fatal("PR comment fragment called the issue comment reader")
	default:
	}
	close(prRelease)
	if recorder := <-prResponse; recorder.Code != http.StatusOK {
		t.Fatalf("PR status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestAPIBoardCardRendersIssueComments(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 7, 4, 15, 30, 0, 0, time.UTC)
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		BoardIssues: []telemetry.Issue{{
			ID:         "i1",
			Identifier: "digitaldrywood/detent#9510",
			ProjectID:  "demo-project",
			Title:      "Kanban demo backlog intake",
			State:      "Backlog",
			Comments: []telemetry.IssueComment{{
				ID:          "IC_1",
				Backend:     connector.BackendGitHub.String(),
				Body:        "Existing issue discussion",
				AuthorLogin: "alice",
				CreatedAt:   &createdAt,
				TargetType:  connector.IssueCommentTargetIssue,
			}},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=demo-project&issue=digitaldrywood%2Fdetent%239510", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"Conversation",
		"Existing issue discussion",
		"alice",
		"GitHub",
		"remote",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("sheet missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestAPIBoardCardRendersIssueCommentEmptyState(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		BoardIssues: []telemetry.Issue{{
			ID:         "i1",
			Identifier: "digitaldrywood/detent#9510",
			ProjectID:  "demo-project",
			Title:      "Kanban demo backlog intake",
			State:      "Backlog",
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=demo-project&issue=digitaldrywood%2Fdetent%239510", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "No issue comments yet.") {
		t.Fatalf("sheet missing issue comment empty state:\n%s", rec.Body.String())
	}
}

func TestAPIBoardCardRendersPullRequestCommentsWhenSupported(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{
		name: "github",
		prThreads: map[string][]connector.IssueComment{
			kanbanPRThreadKey("digitaldrywood/frontend", 42): {{
				ID:          "PRC_1",
				Backend:     connector.BackendGitHub.String(),
				Body:        "Reviewed implementation details",
				AuthorLogin: "reviewer",
				TargetType:  connector.IssueCommentTargetPullRequest,
			}},
		},
	}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, &kanbanPRCommentReaderConnector{kanbanActionConnector: actionConnector})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_scope",
			Identifier: "digitaldrywood/detent#42",
			ProjectID:  "detent",
			Title:      "Scoped sheet card",
			State:      "Todo",
			PullRequest: &telemetry.PullRequest{
				Number: 42,
				URL:    "https://github.com/digitaldrywood/frontend/pull/42",
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=digitaldrywood%2Fdetent%2342&scope=project&actions=board", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		`data-kanban-comment-tab="pr"`,
		"PR comments",
		"Loading PR comments",
		"Comment · PR",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("sheet missing %q:\n%s", want, rec.Body.String())
		}
	}
	fragment := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/conversation?project=detent&issue=digitaldrywood%2Fdetent%2342&scope=project&actions=board&target=pr", http.StatusOK)
	if !strings.Contains(fragment, "Reviewed implementation details") {
		t.Fatalf("project PR comments missing hydrated content:\n%s", fragment)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=digitaldrywood%2Fdetent%2342&actions=board", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fleet status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		`data-kanban-comment-tab="pr"`,
		"PR comments",
		"Loading PR comments",
		"Comment · PR",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("fleet sheet missing %q:\n%s", want, rec.Body.String())
		}
	}
	fragment = requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/conversation?project=detent&issue=digitaldrywood%2Fdetent%2342&actions=board&target=pr", http.StatusOK)
	if !strings.Contains(fragment, "Reviewed implementation details") {
		t.Fatalf("fleet PR comments missing hydrated content:\n%s", fragment)
	}
}

func TestAPIBoardCardHidesPullRequestCommentsWhenUnsupported(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, &kanbanActionConnector{name: "local"})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_scope",
			Identifier: "digitaldrywood/detent#42",
			ProjectID:  "detent",
			Title:      "Scoped sheet card",
			State:      "Todo",
			PullRequest: &telemetry.PullRequest{
				Number: 42,
				URL:    "https://github.com/digitaldrywood/frontend/pull/42",
			},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=digitaldrywood%2Fdetent%2342&scope=project&actions=board", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, unwanted := range []string{
		`data-kanban-comment-tab="pr"`,
		"PR comments",
		"Comment · PR",
	} {
		if strings.Contains(rec.Body.String(), unwanted) {
			t.Fatalf("unsupported PR comments sheet contains %q:\n%s", unwanted, rec.Body.String())
		}
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=digitaldrywood%2Fdetent%2342&actions=board", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fleet status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, unwanted := range []string{
		`data-kanban-comment-tab="pr"`,
		"PR comments",
		"Comment · PR",
	} {
		if strings.Contains(rec.Body.String(), unwanted) {
			t.Fatalf("unsupported fleet PR comments sheet contains %q:\n%s", unwanted, rec.Body.String())
		}
	}
}

func TestAPIBoardCardScopesDemoProjectSheets(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{Demo: web.DemoConfig{Mode: "screenshots"}}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	// A card opened from an integration-mode demo project board must keep its
	// project-scoped Move/Remove actions, not fall back to fleet-scoped data.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=dogfood&issue=digitaldrywood%2Fdetent-core%235251&scope=project&actions=board", nil)
	req.Header.Set(web.DemoScenarioHeader, "kanban-full-integration")
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kanban_board=project") {
		t.Fatalf("demo project sheet should keep project-scoped actions:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Project not found.") {
		t.Fatalf("demo project sheet should not use live project hydration:\n%s", rec.Body.String())
	}
}

func TestAPIBoardCardPreservesProjectScope(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
		AllowedTransitions: map[string][]string{
			"Todo": {"In Progress"},
		},
	}, &kanbanActionConnector{name: "github"})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "I_scope",
				Identifier: "digitaldrywood/detent#42",
				ProjectID:  "detent",
				Title:      "Scoped sheet card",
				State:      "Todo",
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=digitaldrywood%2Fdetent%2342&scope=project&actions=board", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kanban_board=project") {
		t.Fatalf("project-scoped sheet should target the project board:\n%s", rec.Body.String())
	}
	// Integration-mode cards keep the operator comment workflow in the sheet.
	if !strings.Contains(rec.Body.String(), "Comment on issue") {
		t.Fatalf("integration-mode sheet should offer a comment action:\n%s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=digitaldrywood%2Fdetent%2342&actions=board", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fleet status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	conversation := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/conversation?project=detent&issue=digitaldrywood%2Fdetent%2342&actions=board&target=issue", http.StatusOK)
	if !strings.Contains(conversation, `name="kanban_board" value="fleet"`) {
		t.Fatalf("fleet conversation should preserve fleet scope for inline actions:\n%s", conversation)
	}
	// The all-project board is draggable, so its sheet offers the same
	// inline move action as a project board sheet.
	if !strings.Contains(rec.Body.String(), "/api/v1/kanban/move") {
		t.Fatalf("fleet sheet should offer move actions:\n%s", rec.Body.String())
	}

	// Without the board-actions flag (opened from Fleet/Overview) the sheet
	// must not offer inline kanban actions that would swap board lanes over
	// the page the user is on.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=digitaldrywood%2Fdetent%2342", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-actions status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "kanban_board=") {
		t.Fatalf("sheet opened without board actions must omit kanban actions:\n%s", rec.Body.String())
	}
}

func TestAPIBoardCardFleetSheetShowsIssueCommentControls(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeIntegration,
	}, &kanbanActionConnector{name: "github"})
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_fleet_comment",
			Identifier: "digitaldrywood/detent#953",
			ProjectID:  "detent",
			Title:      "Fleet comment card",
			State:      "Todo",
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=digitaldrywood%2Fdetent%23953&actions=board", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"Fleet comment card",
		"Comment on issue",
		"Loading issue comments",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("fleet sheet missing %q:\n%s", want, rec.Body.String())
		}
	}
	conversation := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/conversation?project=detent&issue=digitaldrywood%2Fdetent%23953&actions=board&target=issue", http.StatusOK)
	for _, want := range []string{`name="kanban_board" value="fleet"`, `name="kanban_thread" value="true"`, `hx-post="/api/v1/kanban/comment"`} {
		if !strings.Contains(conversation, want) {
			t.Fatalf("fleet conversation missing %q:\n%s", want, conversation)
		}
	}
}

func TestAPIBoardCardFleetReadOnlyShowsCommentsWithoutWriteControls(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	actionConnector := &kanbanActionConnector{
		name: "github",
		issueComments: map[string][]connector.IssueComment{
			"I_read_only_comment": {{
				ID:          "IC_read_only",
				Backend:     connector.BackendGitHub.String(),
				Body:        "Existing read-only discussion",
				AuthorLogin: "alice",
				TargetType:  connector.IssueCommentTargetIssue,
			}},
		},
	}
	mustSetKanbanProject(t, deps.Registry, "detent", workflowconfig.Kanban{
		Mode: workflowconfig.KanbanModeReadOnly,
	}, actionConnector)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
		Projects: []telemetry.ProjectSnapshot{
			{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
		},
		BoardIssues: []telemetry.Issue{{
			ID:         "I_read_only_comment",
			Identifier: "digitaldrywood/detent#954",
			ProjectID:  "detent",
			Title:      "Read-only fleet comment card",
			State:      "Todo",
			Comments: []telemetry.IssueComment{{
				ID:          "IC_read_only",
				Backend:     connector.BackendGitHub.String(),
				Body:        "Existing read-only discussion",
				AuthorLogin: "alice",
				TargetType:  connector.IssueCommentTargetIssue,
			}},
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/board/card?project=detent&issue=digitaldrywood%2Fdetent%23954&actions=board", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"Read-only fleet comment card",
		"Loading issue comments",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("read-only fleet sheet missing %q:\n%s", want, rec.Body.String())
		}
	}
	conversation := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/conversation?project=detent&issue=digitaldrywood%2Fdetent%23954&actions=board&target=issue", http.StatusOK)
	for _, want := range []string{"Existing read-only discussion", "alice"} {
		if !strings.Contains(conversation, want) {
			t.Fatalf("read-only fleet conversation missing %q:\n%s", want, conversation)
		}
	}
	for _, unwanted := range []string{
		"Comment on issue",
		`name="kanban_thread" value="true"`,
		`hx-post="/api/v1/kanban/comment"`,
	} {
		if strings.Contains(rec.Body.String(), unwanted) || strings.Contains(conversation, unwanted) {
			t.Fatalf("read-only fleet sheet contains %q:\n%s\n%s", unwanted, rec.Body.String(), conversation)
		}
	}
}
