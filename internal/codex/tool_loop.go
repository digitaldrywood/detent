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
	observer         progresscheck.ToolLoop
	thread, turn     string
	workspace        string
	logger           *slog.Logger
	source           func(context.Context, string) string
	probeContext     context.Context
	sequence         uint64
	decisionID       string
	observedDecision uint64
	active           map[string]uint64
	completed        []string
	parallel         bool
	uncertain        bool
	pendingSteer     progresscheck.ToolLoopEvidence
	steerSequence    uint64
	record           func(tracker.ExecutionLoopIntervention)
	interrupted      progresscheck.ToolLoopEvidence
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
	if l.uncertain || l.interrupted.Action != "" || message.Method != "item/started" && message.Method != "item/completed" {
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
		if len(l.active) == 0 && item.ID != l.decisionID {
			l.decisionID = item.ID
			l.sequence++
		}
		return nil
	}
	if !toolItemType(item.Type) || item.Source == "userShell" || slices.Contains(l.completed, item.ID) {
		return nil
	}
	decision, started := l.active[item.ID]
	if message.Method == "item/started" {
		if started {
			return nil
		}
		if len(l.active) == progresscheck.ToolLoopWindow {
			l.uncertain = true
			return nil
		}
		l.parallel = l.parallel || len(l.active) != 0
		if l.active == nil {
			l.active = make(map[string]uint64)
		}
		l.active[item.ID] = l.sequence
		return nil
	}
	if len(l.completed) == progresscheck.ToolLoopWindow {
		copy(l.completed, l.completed[1:])
		l.completed = l.completed[:progresscheck.ToolLoopWindow-1]
	}
	l.completed = append(l.completed, item.ID)
	if !started {
		l.observer.Observe(l.sequence, "")
		l.observedDecision = l.sequence
		l.parallel = len(l.active) != 0
		return nil
	}
	delete(l.active, item.ID)
	if len(l.active) != 0 {
		return nil
	}
	parallel := l.parallel
	l.parallel = false
	if decision == 0 || decision != l.sequence || decision == l.observedDecision {
		return nil
	}
	l.observedDecision = decision
	var fingerprint string
	if !parallel {
		if experiment, ok := item.experiment(l.workspace); ok {
			probeCtx, cancel := context.WithTimeout(l.probeContext, time.Second)
			experiment.Source = l.source(probeCtx, l.workspace)
			cancel()
			fingerprint = experiment.Fingerprint()
		}
	}
	evidence := l.observer.Observe(l.sequence, fingerprint)
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
