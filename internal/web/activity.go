package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/activity"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	defaultBoardActivityLimit = 50
	maxBoardActivityLimit     = 200
	boardSessionIdleTimeout   = 30 * time.Minute
	boardSessionHeartbeat     = 15 * time.Second
)

func (s *Server) apiBoardActivity(c echo.Context) error {
	request := boardActivityRequestFromContext(c)
	snapshot := s.latestSnapshot(c.Request().Context())
	issue := boardActivityIssue(snapshot, request)
	data := s.boardActivityData(c.Request().Context(), snapshot, issue, request)
	return render(c, templates.BoardActivityPanel(data))
}

func (s *Server) apiBoardActivityEvents(c echo.Context) error {
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return echo.NewHTTPError(http.StatusInternalServerError, "streaming unsupported")
	}
	request := boardActivityRequestFromContext(c)
	ctx := c.Request().Context()
	subscription, err := s.hub.Subscribe(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return echo.NewHTTPError(http.StatusServiceUnavailable, "event hub unavailable").SetInternal(err)
	}
	defer subscription.Close()

	response := c.Response()
	prepareSSEResponse(response)
	flusher.Flush()
	stream := newSSEStream(s.logger, s.sseMetricsInterval)
	if err := s.sendBoardActivity(ctx, response, flusher, stream, request, s.latestSnapshot(ctx)); err != nil {
		return err
	}
	ticker := time.NewTicker(s.tickEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case snapshot, ok := <-subscription.C():
			if !ok {
				return nil
			}
			if err := s.sendBoardActivity(ctx, response, flusher, stream, request, snapshot); err != nil {
				return err
			}
		case <-ticker.C:
			if err := s.sendBoardActivity(ctx, response, flusher, stream, request, s.latestSnapshot(ctx)); err != nil {
				return err
			}
		}
	}
}

func (s *Server) sendBoardActivity(
	ctx context.Context,
	response *echo.Response,
	flusher http.Flusher,
	stream *sseStream,
	request boardActivityRequest,
	snapshot telemetry.Snapshot,
) error {
	issue := boardActivityIssue(snapshot, request)
	data := s.boardActivityData(ctx, snapshot, issue, request)
	sent, err := stream.sendComponent(ctx, response.Writer, "board-activity", templates.BoardActivityContent(data), 0)
	if err != nil {
		return err
	}
	if sent {
		flusher.Flush()
	}
	return nil
}

func (s *Server) apiBoardSession(c echo.Context) error {
	request := boardActivityRequestFromContext(c)
	snapshot := s.latestSnapshot(c.Request().Context())
	issue := boardActivityIssue(snapshot, request)
	return render(c, templates.BoardLiveSession(s.boardSessionData(c.Request().Context(), snapshot, issue, request.ProjectID)))
}

func (s *Server) apiBoardSessionHistory(c echo.Context) error {
	request := boardActivityRequestFromContext(c)
	offset, err := strconv.Atoi(strings.TrimSpace(c.QueryParam("offset")))
	if err != nil {
		offset = 0
	}
	if offset < 0 {
		offset = 0
	}
	limit, err := strconv.Atoi(strings.TrimSpace(c.QueryParam("limit")))
	if err != nil {
		limit = 0
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	ctx := c.Request().Context()
	snapshot := s.latestSnapshot(ctx)
	issue := boardActivityIssue(snapshot, request)
	session := s.boardSessionData(ctx, snapshot, issue, request.ProjectID)
	data := templates.BoardSessionHistoryData{Session: session, Offset: offset, Limit: limit}
	state, err := s.latestIssueAgentSession(ctx, issue)
	if err != nil {
		data.Error = "Rollout history is unavailable for this session."
	} else {
		page, pageErr := s.history.Page(ctx, activity.HistoryQuery{
			BackendKind:       state.AgentBackendKind,
			ProviderThreadID:  state.ProviderThreadID,
			ProviderSessionID: state.ProviderSessionID,
			Offset:            offset,
			Limit:             limit,
		})
		if pageErr != nil {
			data.Error = "Rollout history could not be read from the worker host."
		} else {
			data.HasMore = page.HasMore
			data.Events = make([]templates.BoardSessionEvent, 0, len(page.Events))
			for _, event := range page.Events {
				data.Events = append(data.Events, boardSessionEvent(event))
			}
		}
	}
	if offset > 0 {
		return render(c, templates.BoardLiveSessionHistoryMore(data))
	}
	return render(c, templates.BoardLiveSessionHistory(data))
}

func (s *Server) apiBoardSessionEvents(c echo.Context) error {
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return echo.NewHTTPError(http.StatusInternalServerError, "streaming unsupported")
	}
	request := boardActivityRequestFromContext(c)
	issue := boardActivityIssue(s.latestSnapshot(c.Request().Context()), request)
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		issueID = strings.TrimSpace(request.Issue)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), boardSessionIdleTimeout)
	defer cancel()
	subscription := s.activity.Subscribe(ctx, activity.Key{ProjectID: request.ProjectID, IssueID: issueID})
	defer subscription.Close()

	response := c.Response()
	prepareSSEResponse(response)
	flusher.Flush()
	stream := newSSEStream(s.logger, s.sseMetricsInterval)
	if backfill := subscription.Backfill(); len(backfill) > 0 {
		events := make([]templates.BoardSessionEvent, 0, len(backfill))
		for _, event := range backfill {
			events = append(events, boardSessionEvent(event))
		}
		if sent, err := stream.sendComponent(ctx, response.Writer, "session-activity", templates.BoardLiveSessionEvents(events), 0); err != nil {
			return err
		} else if sent {
			flusher.Flush()
		}
	}
	heartbeat := time.NewTicker(boardSessionHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-subscription.C():
			if !ok {
				return nil
			}
			if _, err := stream.sendComponent(ctx, response.Writer, "session-activity", templates.BoardLiveSessionEvent(boardSessionEvent(event)), 0); err != nil {
				return err
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := response.Writer.Write([]byte(": keepalive\n\n")); err != nil {
				return err
			}
			flusher.Flush()
		}
	}
}

func prepareSSEResponse(response *echo.Response) {
	response.Header().Set(echo.HeaderContentType, "text/event-stream; charset=utf-8")
	response.Header().Set(echo.HeaderCacheControl, "no-cache")
	response.Header().Set("Connection", "keep-alive")
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
}

type boardActivityRequest struct {
	ProjectID  string
	Issue      string
	Identifier string
	Verbose    bool
	Limit      int
}

func boardActivityRequestFromContext(c echo.Context) boardActivityRequest {
	limit, err := strconv.Atoi(strings.TrimSpace(c.QueryParam("limit")))
	if err != nil {
		limit = 0
	}
	if limit <= 0 {
		limit = defaultBoardActivityLimit
	}
	if limit > maxBoardActivityLimit {
		limit = maxBoardActivityLimit
	}
	return boardActivityRequest{
		ProjectID:  strings.TrimSpace(c.QueryParam("project")),
		Issue:      strings.TrimSpace(c.QueryParam("issue")),
		Identifier: strings.TrimSpace(c.QueryParam("identifier")),
		Verbose:    c.QueryParam("verbose") == "1",
		Limit:      limit,
	}
}

func boardActivityIssue(snapshot telemetry.Snapshot, request boardActivityRequest) telemetry.Issue {
	candidates := make([]telemetry.Issue, 0, len(snapshot.BoardIssues)+len(snapshot.Pipeline)+len(snapshot.Running))
	candidates = append(candidates, snapshot.BoardIssues...)
	candidates = append(candidates, snapshot.Pipeline...)
	for _, running := range snapshot.Running {
		candidates = append(candidates, running.Issue)
	}
	for _, issue := range candidates {
		if request.ProjectID != "" && issue.ProjectID != "" && request.ProjectID != issue.ProjectID {
			continue
		}
		if activityIssueMatches(issue, request.Issue) || activityIssueMatches(issue, request.Identifier) {
			return issue
		}
	}
	return telemetry.Issue{ID: request.Issue, Identifier: request.Identifier, ProjectID: request.ProjectID}
}

func activityIssueMatches(issue telemetry.Issue, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if value == strings.TrimSpace(issue.ID) || value == strings.TrimSpace(issue.Identifier) || value == strings.TrimSpace(issue.URL) {
		return true
	}
	if issue.Number > 0 {
		number := strconv.Itoa(issue.Number)
		return value == number || value == "#"+number
	}
	return false
}

func (s *Server) boardActivityData(ctx context.Context, snapshot telemetry.Snapshot, issue telemetry.Issue, request boardActivityRequest) templates.BoardActivityData {
	data := templates.BoardActivityData{
		ProjectID:  firstActivityValue(request.ProjectID, issue.ProjectID),
		IssueID:    firstActivityValue(issue.ID, request.Issue),
		Identifier: firstActivityValue(issue.Identifier, request.Identifier),
		IssueURL:   issue.URL,
		Verbose:    request.Verbose,
		Limit:      request.Limit,
	}
	events := make([]templates.BoardActivityEvent, 0, request.Limit+16)
	if activityStore, ok := s.store.(store.ActivityStore); ok {
		stored, err := activityStore.ListIssueActivity(ctx, store.IssueActivityQuery{
			ProjectID:      data.ProjectID,
			IssueID:        data.IssueID,
			Identifier:     data.Identifier,
			IssueURL:       data.IssueURL,
			IncludeVerbose: request.Verbose,
			Limit:          request.Limit + 1,
		})
		if err != nil {
			data.Error = "Runtime history is temporarily unavailable; live events are still shown."
		} else {
			data.HasMore = len(stored) > request.Limit
			if data.HasMore {
				stored = stored[:request.Limit]
			}
			for _, event := range stored {
				events = append(events, boardStoredActivityEvent(event))
			}
		}
	}
	events = append(events, boardSnapshotActivityEvents(snapshot, issue, request.Verbose)...)
	sort.SliceStable(events, func(i int, j int) bool {
		if events[i].At.Equal(events[j].At) {
			return events[i].ID > events[j].ID
		}
		return events[i].At.After(events[j].At)
	})
	events = uniqueBoardActivity(events)
	if len(events) > request.Limit {
		events = events[:request.Limit]
		data.HasMore = true
	}
	data.Events = events
	if data.Limit >= maxBoardActivityLimit {
		data.HasMore = false
	}
	return data
}

func boardStoredActivityEvent(event store.IssueActivityEvent) templates.BoardActivityEvent {
	title := humanizeActivity(event.Name)
	detail := strings.TrimSpace(event.Detail)
	switch event.Source {
	case "scheduler":
		title = "Dispatch " + strings.ToLower(strings.TrimSpace(event.Name))
	case "workflow":
		if event.Kind == string(store.WorkflowPhaseTypeLane) {
			title = "State transition"
			if detail != "" {
				detail += " → " + event.Name
			} else {
				detail = event.Name
			}
		} else {
			title = humanizeActivity(event.Kind) + " · " + humanizeActivity(event.Name)
		}
	case "work_attempt":
		title = "Attempt " + event.Name
	case "session":
		title = "Worker session " + event.Name
	case "usage":
		title = "Turn usage"
	}
	return templates.BoardActivityEvent{
		ID:            event.ID,
		At:            event.At,
		Kind:          event.Kind,
		Title:         title,
		Detail:        detail,
		Reason:        strings.TrimSpace(event.Reason),
		Status:        strings.TrimSpace(event.Status),
		Model:         strings.TrimSpace(event.Model),
		AttemptNumber: event.AttemptNumber,
		SessionID:     event.SessionID,
		Turns:         event.Turns,
		TotalTokens:   event.TotalTokens,
		Verbose:       event.Verbose,
	}
}

func boardSnapshotActivityEvents(snapshot telemetry.Snapshot, issue telemetry.Issue, verbose bool) []templates.BoardActivityEvent {
	events := make([]templates.BoardActivityEvent, 0, 16)
	for _, decision := range snapshot.SchedulerDecisions {
		if !snapshotActivityMatches(decision.ProjectID, decision.IssueID, decision.Identifier, issue) {
			continue
		}
		events = append(events, templates.BoardActivityEvent{
			ID:            fmt.Sprintf("scheduler:%d", decision.ID),
			At:            decision.DecisionAt,
			Kind:          "decision",
			Title:         "Dispatch " + strings.ToLower(decision.Result),
			Detail:        strings.TrimSpace(decision.Lane),
			Reason:        firstActivityValue(decision.WaitReason, decision.Reason),
			Status:        decision.Result,
			AttemptNumber: decision.AttemptNumber,
		})
	}
	for _, running := range snapshot.Running {
		if !snapshotActivityMatches(running.ProjectID, running.ID, running.Identifier, issue) {
			continue
		}
		for index, event := range running.RecentEvents {
			if event.Event == "agent_message_delta" || strings.HasPrefix(event.Event, "tool_") {
				continue
			}
			isVerbose := event.Event == "token_usage"
			if isVerbose && !verbose {
				continue
			}
			events = append(events, templates.BoardActivityEvent{
				ID:          fmt.Sprintf("live:%d:%d", event.At.UnixNano(), index),
				At:          event.At,
				Kind:        event.Event,
				Title:       humanizeActivity(event.Event),
				Detail:      event.Message,
				Model:       running.RuntimeIdentity.Model(),
				TotalTokens: running.Tokens.Total,
				Verbose:     isVerbose,
			})
		}
	}
	if issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() {
		events = append(events, templates.BoardActivityEvent{
			ID:     "snapshot:state:" + strconv.FormatInt(issue.StageUpdatedAt.UnixNano(), 10),
			At:     *issue.StageUpdatedAt,
			Kind:   "lane",
			Title:  "Current state",
			Detail: issue.State,
			Status: issue.State,
		})
	}
	events = append(events, boardDecisionMetadataEvents(snapshot.GeneratedAt, issue)...)
	events = append(events, boardWorkpadEvents(issue)...)
	events = append(events, boardMergeEvents(issue)...)
	for index, event := range snapshot.Events {
		if !activityMessageMatchesIssue(event.Message, issue) || !meaningfulGlobalActivity(event.Event) {
			continue
		}
		events = append(events, templates.BoardActivityEvent{
			ID:     fmt.Sprintf("orchestrator:%d:%d", event.At.UnixNano(), index),
			At:     event.At,
			Kind:   event.Event,
			Title:  humanizeActivity(event.Event),
			Detail: event.Message,
			Status: event.Event,
		})
	}
	return events
}

func boardDecisionMetadataEvents(at time.Time, issue telemetry.Issue) []templates.BoardActivityEvent {
	events := make([]templates.BoardActivityEvent, 0, 2)
	if reason := strings.TrimSpace(issue.Metadata["detent.dispatch_skip_reason"]); reason != "" {
		events = append(events, templates.BoardActivityEvent{ID: "metadata:dispatch-skip", At: at, Kind: "decision", Title: "Dispatch skipped", Reason: reason, Status: "skipped"})
	}
	action := strings.TrimSpace(issue.Metadata["detent.auto_promote_action"])
	reason := strings.TrimSpace(issue.Metadata["detent.auto_promote_reason"])
	if action != "" || reason != "" {
		events = append(events, templates.BoardActivityEvent{ID: "metadata:auto-promote", At: at, Kind: "gate", Title: "Auto-promote " + humanizeActivity(action), Reason: reason, Status: action})
	}
	return events
}

func boardWorkpadEvents(issue telemetry.Issue) []templates.BoardActivityEvent {
	events := make([]templates.BoardActivityEvent, 0, 1)
	for _, comment := range issue.Comments {
		if !strings.Contains(strings.ToLower(comment.Body), "## codex workpad") {
			continue
		}
		at := comment.UpdatedAt
		if at == nil {
			at = comment.CreatedAt
		}
		if at == nil {
			continue
		}
		events = append(events, templates.BoardActivityEvent{ID: "workpad:" + comment.ID, At: *at, Kind: "workpad", Title: "Workpad updated", Detail: comment.AuthorLogin, Status: "updated"})
	}
	return events
}

func boardMergeEvents(issue telemetry.Issue) []templates.BoardActivityEvent {
	if issue.MergeTiming == nil {
		return nil
	}
	values := []struct {
		id     string
		at     *time.Time
		title  string
		status string
	}{
		{id: "entered", at: issue.MergeTiming.EnteredMergingAt, title: "Entered merge train", status: "started"},
		{id: "rebase", at: issue.MergeTiming.BaseRefreshStartedAt, title: "Base refresh started", status: "started"},
		{id: "ci-wait", at: issue.MergeTiming.CIWaitStartedAt, title: "CI wait started", status: "pending"},
		{id: "ci-finished", at: issue.MergeTiming.CIWaitFinishedAt, title: "CI wait finished", status: "completed"},
		{id: "merged", at: issue.MergeTiming.MergedAt, title: "Pull request merged", status: "completed"},
		{id: "failed", at: issue.MergeTiming.MergeFailedAt, title: "Merge failed", status: "failed"},
	}
	events := make([]templates.BoardActivityEvent, 0, len(values))
	for _, value := range values {
		if value.at == nil {
			continue
		}
		events = append(events, templates.BoardActivityEvent{ID: "merge:" + value.id, At: *value.at, Kind: "merge", Title: value.title, Detail: issue.MergeTiming.MergeFailureReason, Status: value.status})
	}
	return events
}

func (s *Server) boardSessionData(ctx context.Context, snapshot telemetry.Snapshot, issue telemetry.Issue, projectID string) templates.BoardSessionData {
	data := templates.BoardSessionData{ProjectID: firstActivityValue(projectID, issue.ProjectID), IssueID: issue.ID, Identifier: issue.Identifier}
	for _, running := range snapshot.Running {
		if snapshotActivityMatches(running.ProjectID, running.ID, running.Identifier, issue) {
			data.Active = true
			data.DetentSessionID = running.DetentSessionID
			data.ProviderSessionID = running.SessionID
			return data
		}
	}
	state, err := s.latestIssueAgentSession(ctx, issue)
	if err == nil && firstActivityValue(state.ProviderThreadID, state.ProviderSessionID) != "" {
		data.DetentSessionID = state.DetentSessionID
		data.ProviderSessionID = firstActivityValue(state.ProviderThreadID, state.ProviderSessionID)
		data.HistoryAvailable = true
	}
	return data
}

func (s *Server) latestIssueAgentSession(ctx context.Context, issue telemetry.Issue) (store.IssueAgentSession, error) {
	activityStore, ok := s.store.(store.ActivityStore)
	if !ok {
		return store.IssueAgentSession{}, store.ErrNotFound
	}
	return activityStore.LatestIssueAgentSession(ctx, store.IssueIdentity{IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL})
}

func boardSessionEvent(event activity.Event) templates.BoardSessionEvent {
	title := strings.TrimSpace(event.Title)
	if title == "" {
		title = humanizeActivity(event.Kind)
	}
	return templates.BoardSessionEvent{
		ID:        fmt.Sprintf("%d-%d-%s-%s", event.DetentSessionID, event.At.UnixNano(), event.Kind, event.ItemID),
		At:        event.At,
		Kind:      event.Kind,
		Title:     title,
		Content:   event.Content,
		Status:    event.Status,
		Model:     event.Model,
		Tokens:    event.TotalTokens,
		Truncated: event.Truncated,
	}
}

func snapshotActivityMatches(projectID string, issueID string, identifier string, issue telemetry.Issue) bool {
	if issue.ProjectID != "" && projectID != "" && issue.ProjectID != projectID {
		return false
	}
	return (issue.ID != "" && issue.ID == issueID) || (issue.Identifier != "" && issue.Identifier == identifier)
}

func activityMessageMatchesIssue(message string, issue telemetry.Issue) bool {
	return (issue.ID != "" && strings.Contains(message, issue.ID)) || (issue.Identifier != "" && strings.Contains(message, issue.Identifier))
}

func meaningfulGlobalActivity(event string) bool {
	event = strings.ToLower(strings.TrimSpace(event))
	for _, part := range []string{"transition", "promote", "breaker", "merge", "gate", "workpad", "dispatch", "recovery", "quota", "capacity"} {
		if strings.Contains(event, part) {
			return true
		}
	}
	return false
}

func uniqueBoardActivity(events []templates.BoardActivityEvent) []templates.BoardActivityEvent {
	seen := make(map[string]struct{}, len(events))
	out := make([]templates.BoardActivityEvent, 0, len(events))
	for _, event := range events {
		key := strings.TrimSpace(event.ID)
		if key == "" {
			key = fmt.Sprintf("%d:%s:%s", event.At.UnixNano(), event.Title, event.Detail)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, event)
	}
	return out
}

func humanizeActivity(value string) string {
	value = strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(value))
	if value == "" {
		return "Activity"
	}
	words := strings.Fields(strings.ToLower(value))
	words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	return strings.Join(words, " ")
}

func firstActivityValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
