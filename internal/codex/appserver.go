package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	initializeRequestID  = 1
	threadStartRequestID = 2
	turnStartRequestID   = 3
	methodNotFoundCode   = -32601

	defaultClientName    = "symphony-orchestrator"
	defaultClientTitle   = "Symphony Orchestrator"
	defaultClientVersion = "0.1.0"

	defaultReadTimeout = 5 * time.Second
	defaultTurnTimeout = time.Hour
)

var (
	ErrResponseError   = errors.New("codex response error")
	ErrInvalidResponse = errors.New("invalid codex response")
	ErrTurnFailed      = errors.New("codex turn failed")
)

type AppServer struct {
	transportFactory TransportFactory
	clientInfo       ClientInfo
	readTimeout      time.Duration
	turnTimeout      time.Duration
}

type AppServerOption func(*AppServer)

type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type RunTurnRequest struct {
	Workspace         string
	Prompt            string
	ApprovalPolicy    any
	ThreadSandbox     string
	TurnSandboxPolicy any
	Model             string
	ModelProvider     string
	ServiceTier       string
}

type RunTurnResult struct {
	ThreadID  string
	TurnID    string
	SessionID string
}

type UpdateHandler func(Update) error

type UpdateType string

const (
	UpdateAgentMessageDelta UpdateType = "agent_message_delta"
	UpdateTokenUsage        UpdateType = "token_usage"
	UpdateRateLimits        UpdateType = "rate_limits"
	UpdateTurnStarted       UpdateType = "turn_started"
	UpdateTurnCompleted     UpdateType = "turn_completed"
)

type Update struct {
	Type       UpdateType
	Method     string
	ThreadID   string
	TurnID     string
	ItemID     string
	Delta      string
	Status     string
	Tokens     TokenUsage
	RateLimits *RateLimitSnapshot
	Payload    json.RawMessage
}

type TokenUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ModelContextWindow    *int64
}

type RateLimitSnapshot struct {
	LimitID              string
	LimitName            string
	Primary              *RateLimitWindow
	Secondary            *RateLimitWindow
	Credits              *CreditsSnapshot
	RateLimitReachedType string
}

type RateLimitWindow struct {
	UsedPercent        float64
	WindowDurationMins *float64
	ResetsAt           *int64
}

type CreditsSnapshot struct {
	HasCredits bool
	Unlimited  bool
	Balance    string
}

func NewAppServer(factory TransportFactory, opts ...AppServerOption) (*AppServer, error) {
	if factory == nil {
		return nil, errors.New("transport factory is nil")
	}

	server := &AppServer{
		transportFactory: factory,
		clientInfo: ClientInfo{
			Name:    defaultClientName,
			Title:   defaultClientTitle,
			Version: defaultClientVersion,
		},
		readTimeout: defaultReadTimeout,
		turnTimeout: defaultTurnTimeout,
	}

	for _, opt := range opts {
		opt(server)
	}
	if server.clientInfo.Name == "" {
		server.clientInfo.Name = defaultClientName
	}
	if server.clientInfo.Version == "" {
		server.clientInfo.Version = defaultClientVersion
	}
	if server.readTimeout <= 0 {
		server.readTimeout = defaultReadTimeout
	}
	if server.turnTimeout <= 0 {
		server.turnTimeout = defaultTurnTimeout
	}

	return server, nil
}

func WithClientInfo(info ClientInfo) AppServerOption {
	return func(server *AppServer) {
		server.clientInfo = info
	}
}

func WithReadTimeout(timeout time.Duration) AppServerOption {
	return func(server *AppServer) {
		server.readTimeout = timeout
	}
}

func WithTurnTimeout(timeout time.Duration) AppServerOption {
	return func(server *AppServer) {
		server.turnTimeout = timeout
	}
}

func (s *AppServer) RunTurn(ctx context.Context, req RunTurnRequest, onUpdate UpdateHandler) (result RunTurnResult, err error) {
	ctx = contextOrBackground(ctx)

	transport, err := s.transportFactory.NewTransport(ctx)
	if err != nil {
		return RunTurnResult{}, fmt.Errorf("start codex app-server transport: %w", err)
	}
	defer func() {
		closeErr := closeTransport(transport, s.readTimeout)
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	if err := s.initialize(ctx, transport, onUpdate); err != nil {
		return RunTurnResult{}, err
	}

	threadID, err := s.startThread(ctx, transport, req, onUpdate)
	if err != nil {
		return RunTurnResult{}, err
	}

	turnID, err := s.startTurn(ctx, transport, req, threadID, onUpdate)
	if err != nil {
		return RunTurnResult{}, err
	}

	result = RunTurnResult{
		ThreadID:  threadID,
		TurnID:    turnID,
		SessionID: threadID + "-" + turnID,
	}
	if err := emitUpdate(Update{
		Type:     UpdateTurnStarted,
		Method:   "turn/start",
		ThreadID: threadID,
		TurnID:   turnID,
		Status:   "started",
	}, onUpdate); err != nil {
		return RunTurnResult{}, err
	}

	if err := s.streamTurn(ctx, transport, onUpdate); err != nil {
		return RunTurnResult{}, err
	}

	return result, nil
}

func (s *AppServer) initialize(ctx context.Context, transport Transport, onUpdate UpdateHandler) error {
	params := map[string]any{
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
		"clientInfo": s.clientInfo,
	}

	if err := sendRequest(ctx, transport, initializeRequestID, "initialize", params); err != nil {
		return err
	}
	if _, err := s.awaitResponse(ctx, transport, initializeRequestID, onUpdate); err != nil {
		return err
	}

	return transport.Send(ctx, Message{
		Method: "initialized",
		Params: json.RawMessage(`{}`),
	})
}

func (s *AppServer) startThread(
	ctx context.Context,
	transport Transport,
	req RunTurnRequest,
	onUpdate UpdateHandler,
) (string, error) {
	params := map[string]any{
		"cwd": req.Workspace,
	}
	setOptional(params, "approvalPolicy", req.ApprovalPolicy)
	if req.ThreadSandbox != "" {
		params["sandbox"] = req.ThreadSandbox
	}
	if req.Model != "" {
		params["model"] = req.Model
	}
	if req.ModelProvider != "" {
		params["modelProvider"] = req.ModelProvider
	}
	if req.ServiceTier != "" {
		params["serviceTier"] = req.ServiceTier
	}

	if err := sendRequest(ctx, transport, threadStartRequestID, "thread/start", params); err != nil {
		return "", err
	}

	result, err := s.awaitResponse(ctx, transport, threadStartRequestID, onUpdate)
	if err != nil {
		return "", err
	}

	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return "", fmt.Errorf("%w: decode thread/start result: %w", ErrInvalidResponse, err)
	}
	if response.Thread.ID == "" {
		return "", fmt.Errorf("%w: thread/start result missing thread id", ErrInvalidResponse)
	}

	return response.Thread.ID, nil
}

func (s *AppServer) startTurn(
	ctx context.Context,
	transport Transport,
	req RunTurnRequest,
	threadID string,
	onUpdate UpdateHandler,
) (string, error) {
	params := map[string]any{
		"threadId": threadID,
		"input": []map[string]any{
			{
				"type":          "text",
				"text":          req.Prompt,
				"text_elements": []any{},
			},
		},
		"cwd": req.Workspace,
	}
	setOptional(params, "approvalPolicy", req.ApprovalPolicy)
	setOptional(params, "sandboxPolicy", req.TurnSandboxPolicy)
	if req.Model != "" {
		params["model"] = req.Model
	}
	if req.ServiceTier != "" {
		params["serviceTier"] = req.ServiceTier
	}

	if err := sendRequest(ctx, transport, turnStartRequestID, "turn/start", params); err != nil {
		return "", err
	}

	result, err := s.awaitResponse(ctx, transport, turnStartRequestID, onUpdate)
	if err != nil {
		return "", err
	}

	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return "", fmt.Errorf("%w: decode turn/start result: %w", ErrInvalidResponse, err)
	}
	if response.Turn.ID == "" {
		return "", fmt.Errorf("%w: turn/start result missing turn id", ErrInvalidResponse)
	}

	return response.Turn.ID, nil
}

func (s *AppServer) awaitResponse(
	ctx context.Context,
	transport Transport,
	requestID int,
	onUpdate UpdateHandler,
) (json.RawMessage, error) {
	for {
		msg, err := receiveWithTimeout(ctx, transport, s.readTimeout)
		if err != nil {
			return nil, fmt.Errorf("wait for %s response: %w", requestName(requestID), err)
		}

		if requestIDMatches(msg.ID, requestID) {
			if msg.Error != nil {
				return nil, fmt.Errorf("%w: %s: %s", ErrResponseError, requestName(requestID), msg.Error.Message)
			}
			if len(msg.Result) == 0 {
				return nil, fmt.Errorf("%w: %s response missing result", ErrInvalidResponse, requestName(requestID))
			}
			return msg.Result, nil
		}

		handled, err := handleServerRequest(ctx, transport, msg)
		if err != nil {
			return nil, err
		}
		if handled {
			continue
		}
		if err := maybeEmitUpdate(msg, onUpdate); err != nil {
			return nil, err
		}
	}
}

func (s *AppServer) streamTurn(ctx context.Context, transport Transport, onUpdate UpdateHandler) error {
	for {
		msg, err := receiveWithTimeout(ctx, transport, s.turnTimeout)
		if err != nil {
			return fmt.Errorf("stream turn: %w", err)
		}

		handled, err := handleServerRequest(ctx, transport, msg)
		if err != nil {
			return err
		}
		if handled {
			continue
		}

		update, ok, err := updateFromMessage(msg)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := emitUpdate(update, onUpdate); err != nil {
			return err
		}
		if update.Type != UpdateTurnCompleted {
			continue
		}
		if update.Status == "" || update.Status == "completed" {
			return nil
		}
		return fmt.Errorf("%w: status %s", ErrTurnFailed, update.Status)
	}
}

func sendRequest(ctx context.Context, transport Transport, id int, method string, params any) error {
	data, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal %s params: %w", method, err)
	}

	return transport.Send(ctx, Message{
		ID:     requestID(id),
		Method: method,
		Params: data,
	})
}

func handleServerRequest(ctx context.Context, transport Transport, msg Message) (bool, error) {
	if msg.Method == "" || len(msg.ID) == 0 {
		return false, nil
	}

	response := Message{
		ID: msg.ID,
	}
	result, ok, err := serverRequestResult(msg.Method)
	if err != nil {
		return true, err
	}
	if ok {
		response.Result = result
	} else {
		response.Error = &RPCError{
			Code:    methodNotFoundCode,
			Message: fmt.Sprintf("unsupported server request: %s", msg.Method),
		}
	}

	if err := transport.Send(ctx, response); err != nil {
		return true, fmt.Errorf("respond to codex server request %s: %w", msg.Method, err)
	}
	return true, nil
}

func serverRequestResult(method string) (json.RawMessage, bool, error) {
	var result any
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		result = map[string]string{"decision": "decline"}
	case "item/permissions/requestApproval":
		result = map[string]any{"permissions": map[string]any{}}
	case "item/tool/requestUserInput":
		result = map[string]any{"answers": map[string]any{}}
	case "mcpServer/elicitation/request":
		result = map[string]any{"action": "decline", "content": nil}
	default:
		return nil, false, nil
	}

	data, err := json.Marshal(result)
	if err != nil {
		return nil, true, fmt.Errorf("marshal %s server request response: %w", method, err)
	}
	return data, true, nil
}

func maybeEmitUpdate(msg Message, onUpdate UpdateHandler) error {
	update, ok, err := updateFromMessage(msg)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return emitUpdate(update, onUpdate)
}

func emitUpdate(update Update, onUpdate UpdateHandler) error {
	if onUpdate == nil {
		return nil
	}
	if err := onUpdate(update); err != nil {
		return fmt.Errorf("handle codex update: %w", err)
	}
	return nil
}

func updateFromMessage(msg Message) (Update, bool, error) {
	switch msg.Method {
	case "item/agentMessage/delta":
		var params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return Update{}, false, fmt.Errorf("%w: decode agent message delta: %w", ErrInvalidResponse, err)
		}
		return Update{
			Type:     UpdateAgentMessageDelta,
			Method:   msg.Method,
			ThreadID: params.ThreadID,
			TurnID:   params.TurnID,
			ItemID:   params.ItemID,
			Delta:    params.Delta,
			Payload:  rawPayload(msg),
		}, true, nil
	case "thread/tokenUsage/updated":
		var params struct {
			ThreadID   string `json:"threadId"`
			TurnID     string `json:"turnId"`
			TokenUsage struct {
				Total              tokenUsageBreakdown `json:"total"`
				ModelContextWindow *int64              `json:"modelContextWindow"`
			} `json:"tokenUsage"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return Update{}, false, fmt.Errorf("%w: decode token usage: %w", ErrInvalidResponse, err)
		}
		return Update{
			Type:     UpdateTokenUsage,
			Method:   msg.Method,
			ThreadID: params.ThreadID,
			TurnID:   params.TurnID,
			Tokens: TokenUsage{
				InputTokens:           params.TokenUsage.Total.InputTokens,
				CachedInputTokens:     params.TokenUsage.Total.CachedInputTokens,
				OutputTokens:          params.TokenUsage.Total.OutputTokens,
				ReasoningOutputTokens: params.TokenUsage.Total.ReasoningOutputTokens,
				TotalTokens:           params.TokenUsage.Total.TotalTokens,
				ModelContextWindow:    params.TokenUsage.ModelContextWindow,
			},
			Payload: rawPayload(msg),
		}, true, nil
	case "account/rateLimits/updated":
		var params struct {
			RateLimits rateLimitSnapshotPayload `json:"rateLimits"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return Update{}, false, fmt.Errorf("%w: decode rate limits: %w", ErrInvalidResponse, err)
		}
		return Update{
			Type:       UpdateRateLimits,
			Method:     msg.Method,
			RateLimits: params.RateLimits.snapshot(),
			Payload:    rawPayload(msg),
		}, true, nil
	case "turn/completed":
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		if len(msg.Params) > 0 {
			if err := json.Unmarshal(msg.Params, &params); err != nil {
				return Update{}, false, fmt.Errorf("%w: decode turn completed: %w", ErrInvalidResponse, err)
			}
		}
		return Update{
			Type:     UpdateTurnCompleted,
			Method:   msg.Method,
			ThreadID: params.ThreadID,
			TurnID:   params.Turn.ID,
			Status:   params.Turn.Status,
			Payload:  rawPayload(msg),
		}, true, nil
	default:
		return Update{}, false, nil
	}
}

type tokenUsageBreakdown struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

type rateLimitSnapshotPayload struct {
	LimitID              string                  `json:"limitId"`
	LimitName            string                  `json:"limitName"`
	Primary              *rateLimitWindowPayload `json:"primary"`
	Secondary            *rateLimitWindowPayload `json:"secondary"`
	Credits              *creditsSnapshotPayload `json:"credits"`
	RateLimitReachedType string                  `json:"rateLimitReachedType"`
}

type rateLimitWindowPayload struct {
	UsedPercent        float64  `json:"usedPercent"`
	WindowDurationMins *float64 `json:"windowDurationMins"`
	ResetsAt           *int64   `json:"resetsAt"`
}

type creditsSnapshotPayload struct {
	HasCredits bool   `json:"hasCredits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance"`
}

func (p rateLimitSnapshotPayload) snapshot() *RateLimitSnapshot {
	return &RateLimitSnapshot{
		LimitID:              p.LimitID,
		LimitName:            p.LimitName,
		Primary:              p.Primary.window(),
		Secondary:            p.Secondary.window(),
		Credits:              p.Credits.credits(),
		RateLimitReachedType: p.RateLimitReachedType,
	}
}

func (p *rateLimitWindowPayload) window() *RateLimitWindow {
	if p == nil {
		return nil
	}
	return &RateLimitWindow{
		UsedPercent:        p.UsedPercent,
		WindowDurationMins: p.WindowDurationMins,
		ResetsAt:           p.ResetsAt,
	}
}

func (p *creditsSnapshotPayload) credits() *CreditsSnapshot {
	if p == nil {
		return nil
	}
	return &CreditsSnapshot{
		HasCredits: p.HasCredits,
		Unlimited:  p.Unlimited,
		Balance:    p.Balance,
	}
}

func receiveWithTimeout(ctx context.Context, transport Transport, timeout time.Duration) (Message, error) {
	ctx = contextOrBackground(ctx)
	if timeout <= 0 {
		return transport.Receive(ctx)
	}

	receiveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return transport.Receive(receiveCtx)
}

func setOptional(params map[string]any, key string, value any) {
	if value == nil {
		return
	}
	if raw, ok := value.(json.RawMessage); ok && len(raw) == 0 {
		return
	}
	params[key] = value
}

func requestID(id int) json.RawMessage {
	return json.RawMessage(strconv.Itoa(id))
}

func requestIDMatches(raw json.RawMessage, id int) bool {
	return bytes.Equal(bytes.TrimSpace(raw), requestID(id))
}

func requestName(id int) string {
	switch id {
	case initializeRequestID:
		return "initialize"
	case threadStartRequestID:
		return "thread/start"
	case turnStartRequestID:
		return "turn/start"
	default:
		return strconv.Itoa(id)
	}
}

func rawPayload(msg Message) json.RawMessage {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return data
}

func closeTransport(transport Transport, timeout time.Duration) error {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if err := transport.Close(ctx); err != nil {
		return fmt.Errorf("close codex app-server transport: %w", err)
	}
	return nil
}
