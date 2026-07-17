package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	kanbanstate "github.com/digitaldrywood/detent/internal/kanban"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
	"github.com/digitaldrywood/detent/internal/workitem"
)

const (
	chatSessionCookieName = "detent_chat_session"
	chatSessionMaxAge     = int((24 * time.Hour) / time.Second)
	defaultChatToolLimit  = 100
)

type chatScenarioContextKey struct{}

type chatMessageRequest struct {
	Message string `form:"message" json:"message"`
}

type chatBoardRow struct {
	ProjectID         string     `json:"project_id,omitempty"`
	IssueID           string     `json:"issue_id"`
	Identifier        string     `json:"identifier,omitempty"`
	Title             string     `json:"title,omitempty"`
	State             string     `json:"state,omitempty"`
	Priority          *int       `json:"priority,omitempty"`
	PriorityName      string     `json:"priority_name,omitempty"`
	BlockedReason     string     `json:"blocked_reason,omitempty"`
	BlockedSource     string     `json:"blocked_source,omitempty"`
	Active            bool       `json:"active,omitempty"`
	Attempt           int        `json:"attempt,omitempty"`
	WorkAttemptID     int64      `json:"work_attempt_id,omitempty"`
	DetentSessionID   int64      `json:"detent_session_id,omitempty"`
	ProviderSessionID string     `json:"provider_session_id,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
}

func (s *Server) apiChatPanel(c echo.Context) error {
	sessionID, err := chatSessionID(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Chat session could not be created").SetInternal(err)
	}
	return render(c, templates.ChatConversation(templates.ChatData{Conversation: s.chat.Conversation(sessionID)}))
}

func (s *Server) apiChatMessage(c echo.Context) error {
	sessionID, err := chatSessionID(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Chat session could not be created").SetInternal(err)
	}
	var request chatMessageRequest
	if err := c.Bind(&request); err != nil {
		return render(c, templates.ChatConversation(templates.ChatData{Conversation: s.chat.Conversation(sessionID), Error: "Enter a valid chat message."}))
	}
	ctx := s.chatContext(c)
	conversation, sendErr := s.chat.Send(ctx, sessionID, request.Message)
	if sendErr != nil {
		s.logger.WarnContext(ctx, "chat turn failed", "error", sendErr)
	}
	errorMessage := ""
	if errors.Is(sendErr, chatpkg.ErrEmptyMessage) {
		errorMessage = "Enter a message before sending."
	} else if errors.Is(sendErr, chatpkg.ErrMessageTooLong) {
		errorMessage = "Message is too long. Keep it under 8 KB."
	}
	return render(c, templates.ChatConversation(templates.ChatData{Conversation: conversation, Error: errorMessage}))
}

func (s *Server) apiChatConfirm(c echo.Context) error {
	sessionID, err := chatSessionID(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Chat session could not be created").SetInternal(err)
	}
	allowed, err := s.authorizeChatAction(c, sessionID, c.Param("action_id"))
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	conversation, actionErr := s.chat.Confirm(s.chatContext(c), sessionID, c.Param("action_id"))
	if actionErr != nil {
		s.logger.WarnContext(c.Request().Context(), "chat action failed", "action_id", c.Param("action_id"), "error", actionErr)
	}
	return render(c, templates.ChatConversation(templates.ChatData{Conversation: conversation}))
}

func (s *Server) authorizeChatAction(c echo.Context, sessionID string, actionID string) (bool, error) {
	action, ok := s.chat.Action(sessionID, actionID)
	if !ok {
		return true, nil
	}
	credential, ok := apiCredentialFromContext(c.Request().Context())
	if !ok {
		return false, c.JSON(http.StatusForbidden, errorResponse("forbidden", "API key context missing"))
	}
	if !apikey.AllowsProject(credential.ProjectIDs, action.ProjectID) {
		return false, c.JSON(http.StatusForbidden, errorResponse("forbidden", "API key is not allowed for project "+action.ProjectID))
	}
	return true, nil
}

func (s *Server) apiChatReject(c echo.Context) error {
	sessionID, err := chatSessionID(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Chat session could not be created").SetInternal(err)
	}
	conversation, actionErr := s.chat.Reject(sessionID, c.Param("action_id"))
	if actionErr != nil {
		s.logger.WarnContext(c.Request().Context(), "chat action rejection failed", "action_id", c.Param("action_id"), "error", actionErr)
	}
	return render(c, templates.ChatConversation(templates.ChatData{Conversation: conversation}))
}

func chatSessionID(c echo.Context) (string, error) {
	if cookie, err := c.Cookie(chatSessionCookieName); err == nil && validChatSessionID(cookie.Value) {
		return cookie.Value, nil
	}
	data := make([]byte, 24)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	id := hex.EncodeToString(data)
	c.SetCookie(&http.Cookie{ // #nosec G124 -- HttpOnly and SameSiteStrict are fixed below; Secure follows the request transport.
		Name:     chatSessionCookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   chatSessionMaxAge,
		HttpOnly: true,
		Secure:   c.Request().TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	return id, nil
}

func validChatSessionID(value string) bool {
	if len(value) != 48 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (s *Server) chatContext(c echo.Context) context.Context {
	ctx := c.Request().Context()
	if s.demo == nil {
		return ctx
	}
	scenario, ok, _ := s.demo.scenario(c.Request())
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, chatScenarioContextKey{}, scenario)
}

func chatScenario(ctx context.Context) demoScenario {
	if ctx == nil {
		return demoScenario{}
	}
	scenario, ok := ctx.Value(chatScenarioContextKey{}).(demoScenario)
	if !ok {
		return demoScenario{}
	}
	return scenario
}

func (s *Server) chatSnapshot(ctx context.Context) telemetry.Snapshot {
	if scenario := chatScenario(ctx); scenario.ID != "" {
		return s.demoDashboardData(ctx, scenario).Snapshot
	}
	return s.latestSnapshot(ctx)
}

func (s *Server) ExecuteTool(ctx context.Context, call chatpkg.ToolCall) (chatpkg.ToolResult, error) {
	switch call.Name {
	case "board_state":
		return s.chatBoardState(ctx, call.Arguments)
	case "fleet_health":
		return s.chatFleetHealth(ctx)
	case "telemetry_usage":
		return s.chatTelemetryUsage(ctx, call.Arguments)
	case "recent_activity":
		return s.chatRecentActivity(ctx, call.Arguments)
	case "propose_move_item":
		return s.chatMoveProposal(ctx, call.Arguments)
	case "propose_set_priority":
		return s.chatPriorityProposal(ctx, call.Arguments)
	case "propose_stop_run":
		return s.chatStopProposal(ctx, call.Arguments)
	case "propose_file_issue":
		return s.chatFileIssueProposal(ctx, call.Arguments)
	default:
		return chatpkg.ToolResult{}, fmt.Errorf("unknown chat tool %q", call.Name)
	}
}

func (s *Server) chatBoardState(ctx context.Context, raw json.RawMessage) (chatpkg.ToolResult, error) {
	var request struct {
		ProjectID string `json:"project_id"`
		State     string `json:"state"`
		Limit     int    `json:"limit"`
	}
	if err := decodeChatToolArguments(raw, &request); err != nil {
		return chatpkg.ToolResult{}, err
	}
	limit := chatToolLimit(request.Limit)
	snapshot := s.chatSnapshot(ctx)
	rows := chatBoardRows(snapshot, request.ProjectID, request.State)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return chatJSONResult(struct {
		GeneratedAt time.Time        `json:"generated_at"`
		Counts      telemetry.Counts `json:"counts"`
		Items       []chatBoardRow   `json:"items"`
	}{GeneratedAt: snapshot.GeneratedAt, Counts: snapshot.Counts, Items: rows})
}

func (s *Server) chatFleetHealth(ctx context.Context) (chatpkg.ToolResult, error) {
	snapshot := s.chatSnapshot(ctx)
	return chatJSONResult(struct {
		GeneratedAt        time.Time                    `json:"generated_at"`
		Auth               telemetry.AuthHealth         `json:"auth"`
		Shutdown           telemetry.Shutdown           `json:"shutdown"`
		Refresh            telemetry.Refresh            `json:"refresh"`
		Counts             telemetry.Counts             `json:"counts"`
		RateLimits         *telemetry.RateLimits        `json:"rate_limits"`
		BackendOutages     []telemetry.BackendOutage    `json:"backend_outages"`
		FailureBreakers    []telemetry.FailureBreaker   `json:"failure_breakers"`
		DispatchRecoveries []telemetry.DispatchRecovery `json:"dispatch_recoveries"`
	}{snapshot.GeneratedAt, snapshot.Auth, snapshot.Shutdown, snapshot.Refresh, snapshot.Counts, snapshot.RateLimits, snapshot.BackendOutages, snapshot.FailureBreakers, snapshot.DispatchRecoveries})
}

func (s *Server) chatTelemetryUsage(ctx context.Context, raw json.RawMessage) (chatpkg.ToolResult, error) {
	var request struct {
		ProjectID string `json:"project_id"`
	}
	if err := decodeChatToolArguments(raw, &request); err != nil {
		return chatpkg.ToolResult{}, err
	}
	snapshot := s.chatSnapshot(ctx)
	projects := slices.Clone(snapshot.Projects)
	if request.ProjectID != "" {
		projects = slices.DeleteFunc(projects, func(candidate telemetry.ProjectSnapshot) bool {
			return !strings.EqualFold(candidate.Project.ID, strings.TrimSpace(request.ProjectID))
		})
	}
	return chatJSONResult(struct {
		GeneratedAt    time.Time                   `json:"generated_at"`
		Projects       []telemetry.ProjectSnapshot `json:"projects"`
		Tokens         telemetry.Tokens            `json:"tokens"`
		Throughput     telemetry.TokenThroughput   `json:"throughput"`
		LifetimeTotals telemetry.LifetimeTotals    `json:"lifetime_totals"`
		Budget         telemetry.Budget            `json:"budget"`
	}{snapshot.GeneratedAt, projects, snapshot.Tokens, snapshot.Throughput, snapshot.LifetimeTotals, snapshot.Budget})
}

func (s *Server) chatRecentActivity(ctx context.Context, raw json.RawMessage) (chatpkg.ToolResult, error) {
	var request struct {
		ProjectID string `json:"project_id"`
		Limit     int    `json:"limit"`
	}
	if err := decodeChatToolArguments(raw, &request); err != nil {
		return chatpkg.ToolResult{}, err
	}
	snapshot := s.chatSnapshot(ctx)
	events := slices.Clone(snapshot.Events)
	completed := slices.Clone(snapshot.Completed)
	if request.ProjectID != "" {
		projectID := strings.TrimSpace(request.ProjectID)
		completed = slices.DeleteFunc(completed, func(candidate telemetry.Completed) bool { return !strings.EqualFold(candidate.ProjectID, projectID) })
	}
	limit := chatToolLimit(request.Limit)
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	if len(completed) > limit {
		completed = completed[:limit]
	}
	return chatJSONResult(struct {
		GeneratedAt time.Time                 `json:"generated_at"`
		Events      []telemetry.ActivityEvent `json:"events"`
		Completed   []telemetry.Completed     `json:"completed"`
	}{snapshot.GeneratedAt, events, completed})
}

func (s *Server) chatMoveProposal(ctx context.Context, raw json.RawMessage) (chatpkg.ToolResult, error) {
	var request struct {
		ProjectID   string `json:"project_id"`
		Identifier  string `json:"identifier"`
		TargetState string `json:"target_state"`
	}
	if err := decodeChatToolArguments(raw, &request); err != nil {
		return chatpkg.ToolResult{}, err
	}
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	request.TargetState = strings.TrimSpace(request.TargetState)
	issue, ok := chatFindIssue(s.chatSnapshot(ctx), request.ProjectID, request.Identifier)
	if !ok {
		return chatpkg.ToolResult{}, errors.New("item was not found on the live board")
	}
	if scenario := chatScenario(ctx); scenario.ID != "" {
		allowed := demoKanbanTransitions([]string{issue.State})[issue.State]
		if !slices.ContainsFunc(allowed, func(state string) bool { return strings.EqualFold(state, request.TargetState) }) {
			return chatpkg.ToolResult{}, errors.New("target state is not an allowed transition")
		}
		return chatpkg.ToolResult{Proposal: &chatpkg.Action{Kind: chatpkg.ActionMoveItem, ProjectID: request.ProjectID, IssueID: issue.ID, Identifier: issue.Identifier, CurrentState: issue.State, TargetState: request.TargetState, ScenarioID: scenario.ID}}, nil
	}
	target, message, _ := s.kanbanActionTarget(request.ProjectID)
	if message != "" {
		return chatpkg.ToolResult{}, errors.New(message)
	}
	if target.kanban.Mode != workflowconfig.KanbanModeIntegration || !kanbanCanMoveCards(target) {
		return chatpkg.ToolResult{}, errors.New("kanban moves are unavailable for this project")
	}
	if !kanbanstate.StateAllowed(target.workflow, request.TargetState) || !target.workflow.KanbanTransitionAllowed(issue.State, request.TargetState) {
		return chatpkg.ToolResult{}, errors.New("target state is not an allowed transition")
	}
	return chatpkg.ToolResult{Proposal: &chatpkg.Action{Kind: chatpkg.ActionMoveItem, ProjectID: request.ProjectID, IssueID: issue.ID, Identifier: issue.Identifier, CurrentState: issue.State, TargetState: request.TargetState}}, nil
}

func (s *Server) chatPriorityProposal(ctx context.Context, raw json.RawMessage) (chatpkg.ToolResult, error) {
	var request struct {
		ProjectID  string `json:"project_id"`
		Identifier string `json:"identifier"`
		Priority   string `json:"priority"`
	}
	if err := decodeChatToolArguments(raw, &request); err != nil {
		return chatpkg.ToolResult{}, err
	}
	action, err := s.priorityProposal(ctx, strings.TrimSpace(request.ProjectID), request.Identifier, request.Priority)
	if err != nil {
		return chatpkg.ToolResult{}, err
	}
	return chatpkg.ToolResult{Proposal: &action}, nil
}

func (s *Server) chatStopProposal(ctx context.Context, raw json.RawMessage) (chatpkg.ToolResult, error) {
	var request struct {
		ProjectID   string `json:"project_id"`
		Identifier  string `json:"identifier"`
		Destination string `json:"destination"`
		Priority    int    `json:"priority"`
		Reason      string `json:"reason"`
	}
	if err := decodeChatToolArguments(raw, &request); err != nil {
		return chatpkg.ToolResult{}, err
	}
	if utf8.RuneCountInString(request.Reason) > orchestrator.StopRunReasonMaxLength {
		return chatpkg.ToolResult{}, errors.New("stop reason is too long")
	}
	running, ok := chatFindRunning(s.chatSnapshot(ctx), request.ProjectID, request.Identifier)
	if !ok {
		return chatpkg.ToolResult{}, errors.New("active run was not found")
	}
	destination, ok := canonicalChatStopDestination(request.Destination)
	if !ok {
		return chatpkg.ToolResult{}, errors.New("stop destination is invalid")
	}
	priorityName := ""
	if destination == orchestrator.StopRunDestinationTodo {
		for _, option := range running.StopPriorityOptions {
			if option.Rank == request.Priority {
				priorityName = option.Name
				break
			}
		}
		if priorityName == "" {
			return chatpkg.ToolResult{}, errors.New("todo destination requires an available priority")
		}
	}
	action := chatpkg.Action{Kind: chatpkg.ActionStopRun, ProjectID: running.ProjectID, IssueID: running.ID, Identifier: running.Identifier, Destination: destination, Priority: priorityName, PriorityRank: request.Priority, Reason: strings.TrimSpace(request.Reason), Attempt: running.Attempt, WorkAttemptID: running.WorkAttemptID, DetentSessionID: running.DetentSessionID, ProviderSessionID: running.SessionID, ScenarioID: chatScenario(ctx).ID}
	return chatpkg.ToolResult{Proposal: &action}, nil
}

func (s *Server) chatFileIssueProposal(ctx context.Context, raw json.RawMessage) (chatpkg.ToolResult, error) {
	var request struct {
		ProjectID   string   `json:"project_id"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		State       string   `json:"state"`
		Labels      []string `json:"labels"`
		Priority    int      `json:"priority"`
	}
	if err := decodeChatToolArguments(raw, &request); err != nil {
		return chatpkg.ToolResult{}, err
	}
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	request.Title = strings.TrimSpace(request.Title)
	request.Description = strings.TrimSpace(request.Description)
	if request.ProjectID == "" || request.Title == "" || request.Description == "" {
		return chatpkg.ToolResult{}, errors.New("project, title, and description are required")
	}
	if chatScenario(ctx).ID == "" {
		if _, ok := s.registry.Get(project.ID(request.ProjectID)); !ok {
			return chatpkg.ToolResult{}, errors.New("project was not found")
		}
	}
	action := chatpkg.Action{Kind: chatpkg.ActionFileIssue, ProjectID: request.ProjectID, Title: request.Title, Description: request.Description, State: strings.TrimSpace(request.State), Labels: trimChatStrings(request.Labels), PriorityRank: request.Priority, ScenarioID: chatScenario(ctx).ID}
	return chatpkg.ToolResult{Proposal: &action}, nil
}

func (s *Server) ExecuteAction(ctx context.Context, action chatpkg.Action) (string, error) {
	if action.ScenarioID != "" {
		return demoChatAction(action), nil
	}
	var result string
	var err error
	switch action.Kind {
	case chatpkg.ActionMoveItem:
		result, err = s.executeChatMove(ctx, action)
	case chatpkg.ActionSetPriority:
		result, err = s.executeChatPriority(ctx, action)
	case chatpkg.ActionStopRun:
		result, err = s.executeChatStop(ctx, action)
	case chatpkg.ActionFileIssue:
		result, action, err = s.executeChatFileIssue(ctx, action)
	default:
		return "", errors.New("chat action is unsupported")
	}
	if err != nil {
		return result, err
	}
	if err := s.recordChatAction(ctx, action, result); err != nil {
		s.logger.WarnContext(ctx, "chat action audit failed", "action", action.Kind, "issue_id", action.IssueID, "error", err)
	}
	return result, nil
}

func (s *Server) executeChatMove(ctx context.Context, action chatpkg.Action) (string, error) {
	form := url.Values{
		"project_id":    {action.ProjectID},
		"issue_id":      {action.IssueID},
		"current_state": {action.CurrentState},
		"target_state":  {action.TargetState},
		"board":         {"project"},
	}
	recorder, err := s.invokeChatHandler(ctx, http.MethodPost, "/api/v1/kanban/move", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil, nil, s.apiKanbanMove)
	if err != nil {
		return "", err
	}
	return chatHandlerMessage(recorder, "Moved "+action.Identifier+" to "+action.TargetState+" via chat.")
}

func (s *Server) executeChatPriority(ctx context.Context, action chatpkg.Action) (string, error) {
	payload, err := json.Marshal(issuePriorityRequest{Priority: action.Priority})
	if err != nil {
		return "", err
	}
	recorder, err := s.invokeChatHandler(ctx, http.MethodPost, "/api/v1/projects/:project_id/issues/:issue_id/priority", bytes.NewReader(payload), echo.MIMEApplicationJSON, []string{"project_id", "issue_id"}, []string{action.ProjectID, action.IssueID}, s.apiIssuePriority)
	if err != nil {
		return "", err
	}
	return chatHandlerMessage(recorder, "Set "+action.Identifier+" priority to "+action.Priority+" via chat.")
}

func (s *Server) executeChatStop(ctx context.Context, action chatpkg.Action) (string, error) {
	payload, err := json.Marshal(stopRunRequestPayload{IssueID: action.IssueID, WorkAttemptID: action.WorkAttemptID, DetentSessionID: action.DetentSessionID, ProviderSessionID: action.ProviderSessionID, Destination: action.Destination, Priority: action.PriorityRank, Reason: action.Reason, Confirm: true})
	if err != nil {
		return "", err
	}
	recorder, err := s.invokeChatHandler(ctx, http.MethodPost, "/api/v1/projects/:project_id/runs/:attempt/stop", bytes.NewReader(payload), echo.MIMEApplicationJSON, []string{"project_id", "attempt"}, []string{action.ProjectID, strconv.Itoa(action.Attempt)}, s.apiStopRun)
	if err != nil {
		return "", err
	}
	return chatHandlerMessage(recorder, "Stopped "+action.Identifier+" and moved it to "+action.Destination+" via chat.")
}

func (s *Server) executeChatFileIssue(ctx context.Context, action chatpkg.Action) (string, chatpkg.Action, error) {
	request := workitem.Request{Title: action.Title, Description: action.Description, State: action.State, Labels: action.Labels}
	if action.PriorityRank > 0 {
		request.Priority = &action.PriorityRank
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return "", action, err
	}
	recorder, err := s.invokeChatHandler(ctx, http.MethodPost, "/api/v1/projects/:project_id/work-items", bytes.NewReader(payload), echo.MIMEApplicationJSON, []string{"project_id"}, []string{action.ProjectID}, s.apiCreateWorkItem)
	if err != nil {
		return "", action, err
	}
	message, err := chatHandlerMessage(recorder, "Filed "+strconv.Quote(action.Title)+" on "+action.ProjectID+" via chat.")
	if err != nil {
		return "", action, err
	}
	var response workitem.Response
	if json.Unmarshal(recorder.Body.Bytes(), &response) == nil {
		action.IssueID = response.ID
		action.Identifier = response.Identifier
	}
	return message, action, nil
}

func (s *Server) invokeChatHandler(ctx context.Context, method string, path string, body io.Reader, contentType string, paramNames []string, paramValues []string, handler echo.HandlerFunc) (*httptest.ResponseRecorder, error) {
	request := httptest.NewRequest(method, path, body).WithContext(ctx)
	request.Header.Set(echo.HeaderContentType, contentType)
	recorder := httptest.NewRecorder()
	ec := s.echo.NewContext(request, recorder)
	ec.SetPath(path)
	ec.SetParamNames(paramNames...)
	ec.SetParamValues(paramValues...)
	if err := handler(ec); err != nil {
		return recorder, err
	}
	return recorder, nil
}

func chatHandlerMessage(recorder *httptest.ResponseRecorder, fallback string) (string, error) {
	if recorder.Code >= http.StatusBadRequest {
		var response struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(recorder.Body.Bytes(), &response) == nil && response.Error.Message != "" {
			return response.Error.Message, errors.New(response.Error.Message)
		}
		return strings.TrimSpace(recorder.Body.String()), fmt.Errorf("chat action handler returned status %d", recorder.Code)
	}
	var response struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(recorder.Body.Bytes(), &response) == nil && strings.TrimSpace(response.Message) != "" {
		return response.Message + " Action source: chat.", nil
	}
	return fallback, nil
}

func (s *Server) recordChatAction(ctx context.Context, action chatpkg.Action, result string) error {
	if s.store == nil || action.IssueID == "" {
		return nil
	}
	now := time.Now().UTC()
	metadata, err := json.Marshal(map[string]string{"source": "chat", "action_id": action.ID, "result": result})
	if err != nil {
		return err
	}
	_, err = s.store.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
		ProjectID:      action.ProjectID,
		IssueID:        action.IssueID,
		Identifier:     action.Identifier,
		PhaseType:      store.WorkflowPhaseTypeOperatorAction,
		PhaseName:      string(action.Kind),
		Reason:         "chat",
		Status:         "succeeded",
		StartedAt:      now,
		FinishedAt:     now,
		EndpointFamily: "web",
		MetadataJSON:   string(metadata),
	})
	return err
}

func (s *Server) demoChatProvider() chatpkg.Provider {
	return chatpkg.ProviderFunc(func(ctx context.Context, request chatpkg.TurnRequest) (chatpkg.TurnResponse, error) {
		threadID := request.ThreadID
		if threadID == "" {
			threadID = "demo-chat-thread"
		}
		if strings.Contains(strings.ToLower(request.Prompt), "move") {
			arguments := json.RawMessage(`{"project_id":"dogfood","identifier":"digitaldrywood/detent-core#5250","target_state":"Todo"}`)
			if _, err := request.Handle(ctx, chatpkg.ToolCall{Name: "propose_move_item", Arguments: arguments}); err != nil {
				return chatpkg.TurnResponse{}, err
			}
			return chatpkg.TurnResponse{ThreadID: threadID, Content: "I prepared a move for digitaldrywood/detent-core#5250 from Backlog to Todo. Confirm it below before Detent changes the board."}, nil
		}
		if _, err := request.Handle(ctx, chatpkg.ToolCall{Name: "board_state", Arguments: json.RawMessage(`{"state":"Blocked"}`)}); err != nil {
			return chatpkg.TurnResponse{}, err
		}
		return chatpkg.TurnResponse{ThreadID: threadID, Content: "Two items are blocked. digitaldrywood/billing-api#5280 is waiting on ledger migration #5200, and digitaldrywood/detent-core#5281 needs operator input after its workspace hook failed."}, nil
	})
}

func demoChatAction(action chatpkg.Action) string {
	switch action.Kind {
	case chatpkg.ActionMoveItem:
		return "Moved " + action.Identifier + " to " + action.TargetState + " via chat."
	case chatpkg.ActionSetPriority:
		return "Set " + action.Identifier + " priority to " + action.Priority + " via chat."
	case chatpkg.ActionStopRun:
		return "Stopped " + action.Identifier + " and moved it to " + action.Destination + " via chat."
	case chatpkg.ActionFileIssue:
		return "Filed " + strconv.Quote(action.Title) + " on " + action.ProjectID + " via chat."
	default:
		return "Action completed via chat."
	}
}

func chatBoardRows(snapshot telemetry.Snapshot, projectID string, state string) []chatBoardRow {
	rows := make(map[string]chatBoardRow)
	add := func(issue telemetry.Issue) {
		key := issue.ProjectID + "\x00" + issue.ID
		rows[key] = chatBoardRow{ProjectID: issue.ProjectID, IssueID: issue.ID, Identifier: issue.Identifier, Title: issue.Title, State: issue.State, Priority: issue.Priority, PriorityName: issue.PriorityName}
	}
	for _, issue := range snapshot.BoardIssues {
		add(issue)
	}
	for _, issue := range snapshot.Pipeline {
		add(issue)
	}
	for _, running := range snapshot.Running {
		add(running.Issue)
		key := running.ProjectID + "\x00" + running.ID
		row := rows[key]
		row.Active = true
		row.Attempt = running.Attempt
		row.WorkAttemptID = running.WorkAttemptID
		row.DetentSessionID = running.DetentSessionID
		row.ProviderSessionID = running.SessionID
		rows[key] = row
	}
	for _, queued := range snapshot.Queue {
		add(queued.Issue)
	}
	for _, blocked := range snapshot.Blocked {
		issue := blocked.Issue
		issue.State = "Blocked"
		add(issue)
		key := blocked.ProjectID + "\x00" + blocked.ID
		row := rows[key]
		row.BlockedReason = blocked.Error
		row.BlockedSource = string(blocked.Source)
		rows[key] = row
	}
	for _, completed := range snapshot.Completed {
		add(completed.Issue)
		key := completed.ProjectID + "\x00" + completed.ID
		row := rows[key]
		completedAt := completed.CompletedAt
		row.CompletedAt = &completedAt
		rows[key] = row
	}
	out := make([]chatBoardRow, 0, len(rows))
	for _, row := range rows {
		if projectID != "" && !strings.EqualFold(row.ProjectID, strings.TrimSpace(projectID)) {
			continue
		}
		if state != "" && !strings.EqualFold(row.State, strings.TrimSpace(state)) {
			continue
		}
		out = append(out, row)
	}
	slices.SortFunc(out, func(left chatBoardRow, right chatBoardRow) int {
		return strings.Compare(left.Identifier, right.Identifier)
	})
	return out
}

func chatFindIssue(snapshot telemetry.Snapshot, projectID string, identifier string) (telemetry.Issue, bool) {
	projectID = strings.TrimSpace(projectID)
	identifier = strings.TrimSpace(identifier)
	for _, issue := range kanbanstate.SnapshotIssues(snapshot) {
		if projectID != "" && !strings.EqualFold(projectID, issue.ProjectID) {
			continue
		}
		if strings.EqualFold(identifier, issue.Identifier) || identifier == issue.ID {
			return issue, true
		}
	}
	return telemetry.Issue{}, false
}

func chatFindRunning(snapshot telemetry.Snapshot, projectID string, identifier string) (telemetry.Running, bool) {
	for _, running := range snapshot.Running {
		if projectID != "" && !strings.EqualFold(strings.TrimSpace(projectID), running.ProjectID) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(identifier), running.Identifier) || strings.TrimSpace(identifier) == running.ID {
			return running, true
		}
	}
	return telemetry.Running{}, false
}

func canonicalChatStopDestination(value string) (string, bool) {
	for _, destination := range []string{orchestrator.StopRunDestinationBlocked, orchestrator.StopRunDestinationBacklog, orchestrator.StopRunDestinationCancelled, orchestrator.StopRunDestinationTodo} {
		if strings.EqualFold(strings.TrimSpace(value), destination) {
			return destination, true
		}
	}
	return "", false
}

func decodeChatToolArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}

func chatJSONResult(value any) (chatpkg.ToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return chatpkg.ToolResult{}, err
	}
	return chatpkg.ToolResult{Content: string(data)}, nil
}

func chatToolLimit(limit int) int {
	if limit <= 0 {
		return defaultChatToolLimit
	}
	return min(limit, 200)
}

func trimChatStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

var _ chatpkg.ToolExecutor = (*Server)(nil)
var _ chatpkg.ActionExecutor = (*Server)(nil)
