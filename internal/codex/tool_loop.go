package codex

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const loopSteerRequestID = 11
const loopInterruptRequestID = 12

type loopToolItem struct {
	ID               string
	Type             string
	Command          string
	CWD              string
	Server           string
	Tool             string
	Status           string
	Source           string
	ExitCode         *int
	AggregatedOutput *string
	Arguments        json.RawMessage
	Result           json.RawMessage
	Error            json.RawMessage
	ContentItems     json.RawMessage
}

type turnToolLoop struct {
	observer       progresscheck.ToolLoop
	thread, turn   string
	workspace      string
	logger         *slog.Logger
	source         func(context.Context, string) string
	probeContext   context.Context
	sequence       uint64
	decisions      []string
	decisionCalls  int
	decisionResult string
	decisionToolID string
	rawToolID      string
	rawCustom      bool
	rawResult      bool
	rawMultiple    bool
	rawClosed      bool
	responses      []string
	active         map[string]struct{}
	completed      []string
	uncertain      bool
	pendingSteer   progresscheck.ToolLoopEvidence
	steerSequence  uint64
	record         func(tracker.ExecutionLoopIntervention)
	interrupted    progresscheck.ToolLoopEvidence
}

func (l *turnToolLoop) observe(ctx context.Context, transport Transport, message Message) error {
	if message.Method == "" && requestIDMatches(message.ID, loopSteerRequestID) && l.pendingSteer.Action != "" {
		if message.Error == nil {
			var response struct {
				TurnID string `json:"turnId"`
			}
			if json.Unmarshal(message.Result, &response) == nil && response.TurnID == l.turn {
				l.observer.Acknowledge(l.pendingSteer.Fingerprint, l.sequence)
				l.report(ctx, l.pendingSteer, l.steerSequence)
			}
		}
		l.pendingSteer = progresscheck.ToolLoopEvidence{}
		return nil
	}
	if message.Method == "" && requestIDMatches(message.ID, loopInterruptRequestID) && message.Error != nil {
		l.interrupted = progresscheck.ToolLoopEvidence{}
		return nil
	}
	if l.uncertain || l.interrupted.Action != "" {
		return nil
	}
	switch message.Method {
	case "rawResponseItem/completed", "rawResponse/completed":
		return l.observeResponse(ctx, transport, message)
	case "item/started", "item/completed":
	default:
		return nil
	}
	var params struct {
		ThreadID string       `json:"threadId"`
		TurnID   string       `json:"turnId"`
		Item     loopToolItem `json:"item"`
	}
	if json.Unmarshal(message.Params, &params) != nil || params.ThreadID != l.thread || params.TurnID != l.turn {
		return nil
	}
	item := params.Item
	if item.ID == "" {
		return nil
	}
	if item.Type == "reasoning" && message.Method == "item/started" {
		if len(l.active) != 0 || slices.Contains(l.decisions, item.ID) {
			return nil
		}
		if err := l.finishDecision(ctx, transport, false); err != nil || l.interrupted.Action != "" {
			return err
		}
		l.rememberID(&l.decisions, item.ID)
		l.nextDecision()
		return nil
	}
	if !toolItemType(item.Type) || item.Source == "userShell" || slices.Contains(l.completed, item.ID) {
		return nil
	}
	_, started := l.active[item.ID]
	if message.Method == "item/started" {
		if started {
			return nil
		}
		if len(l.active) == progresscheck.ToolLoopWindow {
			l.uncertain = true
			return nil
		}
		if l.active == nil {
			l.active = make(map[string]struct{})
		}
		l.active[item.ID] = struct{}{}
		l.decisionCalls++
		return nil
	}
	l.rememberID(&l.completed, item.ID)
	if !started {
		l.decisionCalls++
		l.decisionResult = ""
		return nil
	}
	delete(l.active, item.ID)
	if l.decisionCalls == 1 {
		if experiment, ok := item.experiment(l.workspace); ok {
			probeCtx, cancel := context.WithTimeout(l.probeContext, time.Second)
			experiment.Source = l.source(probeCtx, l.workspace)
			cancel()
			l.decisionResult = experiment.Fingerprint()
			l.decisionToolID = item.ID
		}
	}
	return l.finishRawDecision(ctx, transport)
}

func (l *turnToolLoop) observeResponse(ctx context.Context, transport Transport, message Message) error {
	var params struct {
		ThreadID   string
		TurnID     string
		ResponseID string
		Item       struct {
			ID     string
			CallID string `json:"call_id"`
			Type   string
		}
	}
	if json.Unmarshal(message.Params, &params) != nil || params.ThreadID != l.thread || params.TurnID != l.turn {
		return nil
	}
	if message.Method == "rawResponse/completed" {
		if params.ResponseID == "" || slices.Contains(l.responses, params.ResponseID) {
			return nil
		}
		l.rememberID(&l.responses, params.ResponseID)
		l.rawClosed = true
		return l.finishRawDecision(ctx, transport)
	}
	switch params.Item.Type {
	case "custom_tool_call_output":
		if l.rawCustom && l.rawToolID != "" && l.rawToolID == params.Item.CallID {
			l.rawResult = true
			return l.finishRawDecision(ctx, transport)
		}
	case "function_call", "local_shell_call", "custom_tool_call", "tool_search_call", "web_search_call", "image_generation_call":
		id := firstNonBlank(params.Item.CallID, params.Item.ID)
		custom := params.Item.Type == "custom_tool_call"
		if l.rawClosed && l.rawToolID != "" && l.rawCustom && custom && l.rawToolID != id && len(l.active) == 0 {
			if err := l.finishDecision(ctx, transport, false); err != nil || l.interrupted.Action != "" {
				return err
			}
			l.nextDecision()
		}
		if l.sequence == 0 && custom {
			l.sequence++
		}
		if id == "" || l.rawToolID != "" && l.rawToolID != id {
			l.rawMultiple = true
		}
		l.rawToolID, l.rawCustom = id, custom
	}
	return nil
}

func (l *turnToolLoop) nextDecision() {
	l.sequence++
	l.decisionCalls = 0
	l.decisionResult, l.decisionToolID, l.rawToolID = "", "", ""
	l.rawCustom, l.rawResult, l.rawMultiple, l.rawClosed = false, false, false, false
}

func (l *turnToolLoop) finishRawDecision(ctx context.Context, transport Transport) error {
	if !l.rawClosed || l.rawMultiple || l.decisionToolID == "" || len(l.active) != 0 {
		return nil
	}
	if l.rawCustom && !l.rawResult || !l.rawCustom && l.rawToolID != l.decisionToolID {
		return nil
	}
	return l.finishDecision(ctx, transport, true)
}

func (l *turnToolLoop) rememberID(ids *[]string, id string) {
	if len(*ids) == progresscheck.ToolLoopWindow {
		copy(*ids, (*ids)[1:])
		*ids = (*ids)[:progresscheck.ToolLoopWindow-1]
	}
	*ids = append(*ids, id)
}

func (l *turnToolLoop) finishDecision(ctx context.Context, transport Transport, confirmed bool) error {
	fingerprint := l.decisionResult
	if l.decisionCalls != 1 || !confirmed && l.rawToolID != "" {
		fingerprint = ""
	}
	return l.intervene(ctx, transport, l.observer.Observe(l.sequence, fingerprint))
}

func (l *turnToolLoop) intervene(ctx context.Context, transport Transport, evidence progresscheck.ToolLoopEvidence) error {
	switch evidence.Action {
	case progresscheck.ToolLoopSteer:
		if l.pendingSteer.Action != "" {
			return nil
		}
		input, _ := turnInputItems("The same tool experiment has produced the same result three times. Compare the result with the active acceptance criterion. Choose one different check that can resolve what remains unknown. Preserve the existing work and report a contradiction if the criterion cannot be satisfied.", nil, "")
		if err := sendRequest(ctx, transport, loopSteerRequestID, "turn/steer", map[string]any{"threadId": l.thread, "expectedTurnId": l.turn, "input": input}); err != nil {
			return err
		}
		l.pendingSteer = evidence
		l.steerSequence = l.sequence
	case progresscheck.ToolLoopStop:
		if err := sendRequest(ctx, transport, loopInterruptRequestID, "turn/interrupt", map[string]string{"threadId": l.thread, "turnId": l.turn}); err != nil {
			return err
		}
		l.interrupted = evidence
	}
	return nil
}

func (l *turnToolLoop) confirmStop(ctx context.Context) error {
	l.report(ctx, l.interrupted, l.sequence)
	return &progresscheck.NoProgressError{Evidence: l.interrupted}
}

func (l *turnToolLoop) report(ctx context.Context, evidence progresscheck.ToolLoopEvidence, sequence uint64) {
	if l.record != nil {
		l.record(tracker.ExecutionLoopIntervention{ThreadID: l.thread, TurnID: l.turn, Action: string(evidence.Action), Fingerprint: evidence.Fingerprint, Period: evidence.Period, Repeats: evidence.Repeats, Sequence: sequence})
	}
	l.logger.InfoContext(ctx, "provider tool loop intervention", "action", evidence.Action, "repeats", evidence.Repeats, "period", evidence.Period, "fingerprint", evidence.Fingerprint, "thread_id", l.thread, "turn_id", l.turn)
}

func (i loopToolItem) experiment(root string) (progresscheck.ToolExperiment, bool) {
	e := progresscheck.ToolExperiment{Tool: i.Type, Status: i.Status, ExitCode: i.ExitCode}
	if i.Source == "unifiedExecInteraction" {
		return e, false
	}
	if i.Status != "completed" && i.Status != "failed" && i.Status != "declined" {
		return e, false
	}
	switch i.Type {
	case "commandExecution":
		dir, err := filepath.Rel(root, i.CWD)
		if err != nil || !filepath.IsLocal(dir) || i.Command == "" || i.ExitCode == nil || i.AggregatedOutput == nil {
			return e, false
		}
		if strings.Contains(*i.AggregatedOutput, "truncated") || strings.Contains(*i.AggregatedOutput, "Process running with session ID") {
			return e, false
		}
		e.WorkspaceDir = dir
		e.Arguments, _ = json.Marshal(i.Command)
		e.Result, _ = json.Marshal(*i.AggregatedOutput)
	case "mcpToolCall", "dynamicToolCall":
		if i.Tool == "" || len(i.Arguments) == 0 {
			return e, false
		}
		e.Tool += "/" + i.Server + "/" + i.Tool
		e.Arguments = i.Arguments
		e.Result = i.Result
		if len(e.Result) == 0 || string(e.Result) == "null" {
			e.Result = i.Error
		}
		if len(e.Result) == 0 || string(e.Result) == "null" {
			e.Result = i.ContentItems
		}
	default:
		return e, false
	}
	return e, len(e.Result) != 0 && string(e.Result) != "null"
}

func newTurnToolLoop(req RunTurnRequest, thread, turn string, logger *slog.Logger) *turnToolLoop {
	if !req.DetectToolLoops {
		return nil
	}
	return &turnToolLoop{thread: thread, turn: turn, workspace: req.Workspace, logger: logger, source: workspace.ToolSourceFingerprint, record: req.RecordLoopIntervention}
}
