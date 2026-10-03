package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/activity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

const (
	activitySpanLimit     = 1024
	activityActionLimit   = 32
	activitySourceLimit   = 64
	activitySnapshotLimit = 64
)

type activityProfileStore interface {
	SaveWorkflowActivityProfile(context.Context, int64, store.WorkflowPhaseEvent, workflowmetrics.ActivityProfile) (int64, error)
}

// Keep queued observations small: AgentUpdate also contains runtime identity,
// token accounting and provider diagnostics that profiling never consumes.
type activityUpdate struct {
	Type                                        AgentUpdateType
	ItemID, TurnID, ThreadID, ProviderSessionID string
	Tool, Command, Delta, Status                string
	ExitCode                                    *int
	NativeActions                               []NativeCommandAction
	ActionsDropped                              int
	CWD                                         string
}

type activityObservation struct {
	update         activityUpdate
	at             time.Time
	head           string
	headAt         time.Time
	nativeSources  map[int]workflowmetrics.InstructionRef
	sourceCoverage map[int]string
}

type activityRecorder struct {
	queue   chan activityObservation
	wake    chan struct{}
	dropped atomic.Uint64
	done    chan struct{}
}

// observe never waits for persistence, file reads, classification or queries.
// Output and prompts are excluded from the queue, except bounded tool inputs
// for classification and fixed validation-wait markers.
func (r *activityRecorder) observe(update AgentUpdate, at time.Time, head string, headAt time.Time) {
	if r == nil {
		return
	}
	if len(update.ItemID) > 512 || len(update.TurnID) > 512 || len(update.Tool) > 512 || len(update.ThreadID) > 512 || len(update.ProviderSessionID) > 512 {
		r.dropped.Add(1)
		return
	}
	var delta string
	switch update.Type {
	case AgentUpdateToolStarted:
		if len(update.Command) > 8192 || len(update.Delta) > 8192 {
			r.dropped.Add(1)
			return
		}
		delta = update.Delta
	case AgentUpdateToolCompleted, AgentUpdateTurnStarted, AgentUpdateTurnCompleted:
	case AgentUpdateToolOutput:
		if update.Tool != "tool_result" {
			text := update.Delta
			if len(text) > 1024 {
				text = text[len(text)-1024:]
			}
			switch activity.ValidationPhase(text) {
			case "waiting_validation":
				delta = "validation_lock"
			case "validating":
				delta = "validation_acquired"
			default:
				return
			}
		}
	default:
		return
	}
	observation := activityObservation{at: at, head: head, headAt: headAt, update: activityUpdate{Type: update.Type, ItemID: update.ItemID, TurnID: update.TurnID, ThreadID: update.ThreadID, ProviderSessionID: update.ProviderSessionID, Tool: update.Tool, Delta: delta, Status: update.Status, ExitCode: update.ExitCode}}
	if update.Type == AgentUpdateToolStarted {
		observation.update.Command = update.Command
	}
	if update.Type == AgentUpdateToolStarted || update.Type == AgentUpdateToolCompleted {
		// Copy a bounded prefix: producer-owned slices must not change queued evidence.
		budget := 8192
		if len(update.CWD) <= 4096 {
			observation.update.CWD = strings.Clone(update.CWD)
			budget -= len(update.CWD)
		}
		if len(update.CWD) > 4096 {
			observation.update.ActionsDropped = len(update.NativeActions)
		} else {
			if len(update.NativeActions) > 0 {
				observation.update.NativeActions = make([]NativeCommandAction, 0, min(len(update.NativeActions), activityActionLimit))
			}
			for i, action := range update.NativeActions {
				size := len(action.Type) + len(action.Command) + len(action.Name) + len(action.Path)
				if i >= activityActionLimit || size > budget {
					observation.update.ActionsDropped = len(update.NativeActions) - i
					break
				}
				budget -= size
				observation.update.NativeActions = append(observation.update.NativeActions, NativeCommandAction{Type: strings.Clone(action.Type), Command: strings.Clone(action.Command), Name: strings.Clone(action.Name), Path: strings.Clone(action.Path)})
			}
		}
	}
	select {
	case r.queue <- observation:
		// Wake once per batch rather than scheduling a consumer per event.
		if len(r.queue) >= 64 {
			select {
			case r.wake <- struct{}{}:
			default:
			}
		}
	default:
		r.dropped.Add(1)
	}
}

func (r *activityRecorder) close() {
	if r != nil {
		close(r.queue)
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

func (r *activityRecorder) finish() {
	if r != nil {
		r.close()
		<-r.done
	}
}

func (r *Runner) startActivityProfile(ctx context.Context, request RunRequest, sessionID int64, workspace string, workflow config.Workflow, stage string) *activityRecorder {
	backend, ok := r.store.(activityProfileStore)
	if !ok || sessionID <= 0 {
		return nil
	}
	recorder := &activityRecorder{queue: make(chan activityObservation, 256), wake: make(chan struct{}, 1), done: make(chan struct{})}
	started := r.now()
	go func() {
		defer close(recorder.done)
		profile := workflowmetrics.ActivityProfile{
			Schema: 1, AttemptID: request.WorkAttemptID, Generation: request.Generation,
			SessionID: sessionID, Stage: stage, StartedAt: started, AsOf: started,
			Status: "running", Coverage: "partial",
			CoverageNotes: []string{
				"native_actions_have_no_individual_timing_or_outcomes",
				"provider_instruction_origin_unavailable",
				"read_requests_are_not_causal_proof",
				"instruction_versions_are_recorder_snapshots_not_provider_read_bytes",
				"instruction_snapshots_limited_to_workspace_regular_files_64_reads_64_sources_256KiB_each",
				"native_actions_limited_to_32_and_8KiB_per_event_4_inferred_candidates_per_action",
				"repeat_counts_limited_to_bounded_fingerprints",
			},
			Spans: make([]workflowmetrics.ActivitySpan, 0, 64),
		}
		profile.AttemptRef = "work_attempt:" + strconv.FormatInt(request.WorkAttemptID, 10)
		if request.WorkAttemptID == 0 {
			profile.AttemptRef = "session:" + strconv.FormatInt(sessionID, 10)
		}
		instructions := loadActivityInstructions(workspace, workflow, started)
		for _, source := range instructions {
			profile.Sources = append(profile.Sources, source.ref)
		}
		snapshots := 0
		event := store.WorkflowPhaseEvent{ProjectID: r.projectID, SessionID: sessionID, IssueID: request.Issue.ID, Identifier: request.Issue.Identifier, IssueURL: request.Issue.URL, PRNumber: pullRequestNumber(request.Issue), PhaseType: workflowmetrics.PhaseTypeAgentActivity, PhaseName: "instruction_activity", StartedAt: started}
		var id int64
		var savedProfile [32]byte
		dirty := true
		persist := func() {
			dropped := recorder.dropped.Swap(0)
			profile.Dropped += dropped
			if !dirty && dropped == 0 {
				return
			}
			dirty = true
			checkpoint := profile
			checkpoint.AsOf = time.Time{}
			encoded, err := json.Marshal(checkpoint)
			if err != nil {
				r.logger.Warn("activity telemetry checkpoint unavailable", "session_id", sessionID, "error", err)
				return
			}
			signature := sha256.Sum256(encoded)
			if signature == savedProfile {
				dirty = false
				return
			}
			profile.AsOf = r.now()
			event.Status = profile.Status
			event.FinishedAt = profile.FinishedAt
			// Retain run attribution while allowing the final checkpoint after cancellation.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			if runtime, ok := request.Execution.(RuntimeExecution); ok {
				if err := runtime.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{LocalAttemptID: request.WorkAttemptID, Generation: request.Generation, Phase: stage, HeartbeatAt: profile.AsOf, Activity: &profile}); err != nil {
					r.logger.Warn("native activity observation unavailable", "session_id", sessionID, "error", err)
				}
			}
			next, err := backend.SaveWorkflowActivityProfile(ctx, id, event, profile)
			if err != nil {
				r.logger.Warn("activity telemetry checkpoint unavailable", "session_id", sessionID, "error", err)
				return
			}
			id = next
			savedProfile = signature
			dirty = false
		}
		persist()
		open := make(map[string]int)
		repeats := make(map[string]int)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-recorder.wake:
			case <-ticker.C:
			}
			draining := true
			for draining {
				select {
				case observation, ok := <-recorder.queue:
					dirty = true
					if !ok {
						if profile.Status == "running" {
							profile.Status = "ended_without_terminal_event"
						}
						profile.FinishedAt = r.now()
						for _, index := range open {
							profile.Spans[index].Outcome = "unobserved"
						}
						persist()
						return
					}
					if observation.update.Type == AgentUpdateToolStarted && len(profile.Spans) >= activitySpanLimit {
						compactActivitySpans(&profile, open)
					}
					snapshotReads := observation.update.Type == AgentUpdateToolStarted && len(profile.Spans) < activitySpanLimit
					if observation.update.Type == AgentUpdateToolCompleted {
						u := observation.update
						key := activityHash(u.ThreadID + "\x00" + u.TurnID + "\x00" + u.ItemID)
						if index, ok := open[key]; ok && len(profile.Spans[index].Actions) == 0 {
							snapshotReads = true
						}
					}
					if snapshotReads {
						snapshotActivityReads(&profile, &instructions, &snapshots, workspace, &observation)
					}
					applyActivityObservation(&profile, open, repeats, instructions, observation)
				default:
					draining = false
				}
			}
			// Consumption batches do not increase the checkpoint write rate.
			if r.now().Sub(profile.AsOf) >= 5*time.Second {
				persist()
			}
		}
	}()
	return recorder
}

type activityInstruction struct {
	ref  workflowmetrics.InstructionRef
	text string
	path string
}

func activityHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func loadActivityInstructions(workspace string, workflow config.Workflow, at time.Time) []activityInstruction {
	sources := []activityInstruction{{ref: workflowmetrics.InstructionRef{Name: "WORKFLOW.md (effective)", Hash: activityHash(workflow.Prompt + workflow.SharedPrompt), Version: workflow.SourceHash, ObservedAt: at}, text: workflow.Prompt + workflow.SharedPrompt}}
	if workflow.AgentsPrompt != "" {
		sources = append(sources, activityInstruction{ref: workflowmetrics.InstructionRef{Name: "AGENTS.md (effective)", Hash: activityHash(workflow.AgentsPrompt), Version: workflow.SourceHash, ObservedAt: at}, text: workflow.AgentsPrompt})
	}
	for _, name := range []string{"WORKFLOW.md", "AGENTS.md", "CLAUDE.md"} {
		file, err := os.Open(filepath.Join(workspace, name))
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(file, 256*1024+1))
		_ = file.Close() //nolint:errcheck // Best effort cleanup of a read-only instruction file.
		if err != nil || len(data) > 256*1024 {
			continue
		}
		sources = append(sources, activityInstruction{ref: workflowmetrics.InstructionRef{Name: name, PathRef: activityHash(name), Evidence: "startup_snapshot", Hash: activityHash(string(data)), ObservedAt: time.Now().UTC()}, text: string(data), path: filepath.Join(workspace, name)})
	}
	return sources
}

func applyActivityObservation(profile *workflowmetrics.ActivityProfile, open map[string]int, repeats map[string]int, sources []activityInstruction, observation activityObservation) {
	u := observation.update
	if u.ThreadID != "" {
		profile.ProviderThreadRef = activityHash(u.ThreadID)
	}
	if u.ProviderSessionID != "" {
		profile.ProviderSessionRef = activityHash(u.ProviderSessionID)
	}
	parent := activityHash(u.ThreadID + "\x00" + u.TurnID)
	if u.Type == AgentUpdateTurnStarted || u.Type == AgentUpdateTurnCompleted {
		if u.Type == AgentUpdateTurnCompleted {
			profile.Status = activityOutcome(u.Status, nil)
		}
		return
	}
	key := activityHash(u.ThreadID + "\x00" + u.TurnID + "\x00" + u.ItemID)
	if u.ItemID == "" {
		profile.Unpaired++
		return
	}
	if u.Type == AgentUpdateToolStarted {
		if _, exists := open[key]; exists {
			profile.Unpaired++
			return
		}
		if len(profile.Spans) >= activitySpanLimit {
			compactActivitySpans(profile, open)
			if len(profile.Spans) >= activitySpanLimit {
				profile.Dropped++
				return
			}
		}
		command := u.Command
		if command == "" {
			command = activityInputCommand(u.Delta)
		}
		activityCommand := command
		if u.Tool == "commandExecution" && u.Delta != "" {
			activityCommand = u.Delta
		}
		kind, evidence := classifyActivity(u.Tool, activityCommand)
		span := workflowmetrics.ActivitySpan{ID: key, ParentID: parent, Kind: kind, Evidence: evidence, Fingerprint: activityHash(u.Tool + "\x00" + command), StartedAt: observation.at, Outcome: "running", Attribution: "unattributed", CausalAttribution: "unknown_provider_origin"}
		span.Head = observation.head
		span.HeadObservedAt = observation.headAt
		if span.Head != "" {
			span.HeadAttribution = "last_observed_workspace_snapshot"
		}
		for _, source := range sources {
			if len(u.NativeActions) > 0 || u.ActionsDropped > 0 {
				break
			}
			if kind == "context_read" && activityReadsSource(activityCommand, source) {
				span.Sources = append(span.Sources, source.ref)
				span.Attribution = "observed_read_request"
			}
			if kind != "context_read" && activityCommand != "" && kind != "unclassified" && kind != "tool_execution" && strings.Contains(source.text, activityCommand) {
				ref := source.ref
				ref.MatchedLine = 1 + strings.Count(source.text[:strings.Index(source.text, activityCommand)], "\n")
				span.Sources = append(span.Sources, ref)
				span.Attribution = "inferred_text_match"
			}
		}
		applyActivityActions(&span, repeats, sources, observation)
		repeatKey := span.Head + "\x00" + span.Fingerprint
		span.Repeat = activityRepeat(repeats, repeatKey)
		open[key] = len(profile.Spans)
		profile.Spans = append(profile.Spans, span)
		return
	}
	index, exists := open[key]
	if !exists {
		profile.Unpaired++
		return
	}
	if u.Type == AgentUpdateToolOutput && u.Tool != "tool_result" {
		// A validation lock is a child interval, not the entire command.
		switch u.Delta {
		case "validation_lock":
			if len(profile.Spans) >= activitySpanLimit {
				compactActivitySpans(profile, open)
			}
			waitKey := key + "/wait"
			if _, exists := open[waitKey]; !exists && len(profile.Spans) < activitySpanLimit {
				open[waitKey] = len(profile.Spans)
				profile.Spans = append(profile.Spans, workflowmetrics.ActivitySpan{ID: waitKey, ParentID: key, Kind: "waiting", Evidence: "validation_lock_marker", StartedAt: observation.at, Outcome: "running", Attribution: "observed", WaitReason: "validation_lock", Repeat: 1})
			}
		case "validation_acquired":
			finishActivityWait(profile, open, key, observation.at)
		}
		return
	}
	if waitIndex, exists := open[key+"/wait"]; exists {
		// Completion bounds the command, but does not reveal when its lock
		// wait ended if the acquired marker was lost.
		profile.Spans[waitIndex].Outcome = "unobserved"
		profile.Unpaired++
		delete(open, key+"/wait")
	}
	span := &profile.Spans[index]
	if len(span.Actions) == 0 {
		applyActivityActions(span, repeats, sources, observation)
	}
	span.FinishedAt = observation.at
	span.Outcome = activityOutcome(u.Status, u.ExitCode)
	span.ExitCode = u.ExitCode
	delete(open, key)
}

// The first available native list describes the interval; repeated completion
// metadata must not count the same actions again or invent new timed spans.
func applyActivityActions(span *workflowmetrics.ActivitySpan, repeats map[string]int, sources []activityInstruction, observation activityObservation) {
	u := observation.update
	if len(u.NativeActions) > 0 || u.ActionsDropped > 0 {
		span.Sources = nil
		span.Attribution = "unattributed"
		span.ActionsDropped = u.ActionsDropped
		for i, action := range u.NativeActions {
			record := activityAction(action, i, sources, observation)
			repeatKey := "action\x00" + span.Head + "\x00" + record.Fingerprint
			record.Repeat = activityRepeat(repeats, repeatKey)
			span.Actions = append(span.Actions, record)
			span.Sources = append(span.Sources, record.Sources...)
			if len(record.Sources) > 0 {
				if span.Attribution == "unattributed" {
					span.Attribution = record.Attribution
				} else if span.Attribution != record.Attribution {
					span.Attribution = "mixed_evidence"
				}
			}
			if i == 0 {
				span.Kind, span.Evidence = record.Kind, record.Evidence
			} else if span.Kind != record.Kind {
				span.Kind, span.Evidence = "unclassified", "mixed_native_actions"
			}
		}
		if u.ActionsDropped > 0 {
			span.Kind, span.Evidence = "unclassified", "partial_native_actions"
		}
	}
	span.WaitReason = ""
	if span.Kind == "waiting" {
		span.WaitReason = span.Evidence
	}
}

func activityRepeat(repeats map[string]int, key string) int {
	if _, exists := repeats[key]; !exists && len(repeats) >= activitySpanLimit*(activityActionLimit+1) {
		return 0
	}
	repeats[key]++
	return repeats[key]
}

func compactActivitySpans(profile *workflowmetrics.ActivityProfile, open map[string]int) {
	var through time.Time
	closed := 0
	for _, span := range profile.Spans {
		if span.FinishedAt.IsZero() {
			continue
		}
		if span.FinishedAt.After(through) {
			through = span.FinishedAt
		}
		closed++
		if closed == activitySpanLimit/4 {
			break
		}
	}
	if closed == 0 {
		return
	}
	for _, index := range open {
		if profile.Spans[index].StartedAt.Before(through) {
			const note = "summarized_intervals_exclude_unfinished_spans"
			if !slices.Contains(profile.CoverageNotes, note) {
				profile.CoverageNotes = append(profile.CoverageNotes, note)
			}
			break
		}
	}
	profile.SummarizeThrough(through)
	kept := profile.Spans[:0]
	for _, span := range profile.Spans {
		if !span.FinishedAt.IsZero() && !span.FinishedAt.After(through) {
			profile.DetailOmitted++
			continue
		}
		if _, pending := open[span.ID]; pending {
			open[span.ID] = len(kept)
		}
		kept = append(kept, span)
	}
	clear(profile.Spans[len(kept):])
	profile.Spans = kept
}

func finishActivityWait(profile *workflowmetrics.ActivityProfile, open map[string]int, key string, at time.Time) {
	waitKey := key + "/wait"
	if index, ok := open[waitKey]; ok {
		profile.Spans[index].FinishedAt = at
		profile.Spans[index].Outcome = "completed"
		delete(open, waitKey)
	}
}

func activityOutcome(status string, exit *int) string {
	if exit != nil {
		if *exit == 0 {
			return "completed"
		}
		return "failed"
	}
	switch status {
	case "completed", "success", "succeeded":
		return "completed"
	case "failed", "error", "timed_out":
		return "failed"
	case "cancelled", "canceled", "interrupted":
		return "cancelled"
	default:
		return "unknown"
	}
}

// Evidence labels are a fixed vocabulary. Never persist raw shell text or tool
// arguments, even when a command appears harmless.
func classifyActivity(tool, command string) (string, string) {
	t := strings.ToLower(tool)
	for _, separator := range []string{"/", ".", "__"} {
		if index := strings.LastIndex(t, separator); index >= 0 {
			t = t[index+len(separator):]
		}
	}
	switch t {
	case "read", "read_file", "read_mcp_resource", "get_file_contents":
		return "context_read", "read_tool"
	case "filechange", "edit", "multiedit", "write", "edit_file", "write_file", "apply_patch":
		return "implementation", "edit_tool"
	case "wait", "write_stdin", "sleep":
		return "waiting", "wait_tool"
	case "exec", "dynamictoolcall":
		return "unclassified", "opaque_tool_execution"
	}
	segments := shellCommandSegments(command)
	kind, evidence := "", ""
	for _, segment := range segments {
		fields := strings.Fields(segment)
		if len(fields) > 0 && filepath.Base(fields[0]) == "env" {
			fields = fields[1:]
			for len(fields) > 1 && fields[0] == "-u" {
				fields = fields[2:]
			}
			if len(fields) > 0 && fields[0] == "--" {
				fields = fields[1:]
			}
		}
		for len(fields) > 0 && strings.Contains(fields[0], "=") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		name := filepath.Base(fields[0])
		nextKind, nextEvidence := "tool_execution", "tool_lifecycle"
		switch name {
		case "cat", "sed", "rg", "head", "tail":
			nextKind, nextEvidence = "context_read", "read_or_search"
		case "go":
			if len(fields) > 1 && (fields[1] == "test" || fields[1] == "vet" || fields[1] == "build") {
				nextKind, nextEvidence = "local_validation", "go_"+fields[1]
			}
		case "make":
			if len(fields) > 1 && (strings.HasPrefix(fields[1], "check") || strings.HasPrefix(fields[1], "test") || fields[1] == "lint" || fields[1] == "build") {
				nextKind, nextEvidence = "local_validation", "make_validation"
				switch fields[1] {
				case "check", "check-fast", "check-invariants", "test", "lint", "build":
					nextEvidence = "make_" + strings.ReplaceAll(fields[1], "-", "_")
				}
			}
		case "golangci-lint":
			nextKind, nextEvidence = "local_validation", "lint_command"
		case "git":
			if len(fields) > 1 {
				switch fields[1] {
				case "rebase", "cherry-pick":
					nextKind, nextEvidence = "rebase", "git_rebase_or_conflict"
				case "merge":
					nextKind, nextEvidence = "merge", "git_merge"
				case "diff", "show":
					nextKind, nextEvidence = "review", "git_review"
				}
			}
		case "gh":
			if len(fields) > 2 && fields[1] == "pr" {
				switch fields[2] {
				case "merge":
					nextKind, nextEvidence = "merge", "gh_pr_merge"
				case "review", "diff":
					nextKind, nextEvidence = "review", "gh_pr_review"
				case "checks":
					nextKind, nextEvidence = "review", "ci_check_observation"
					if strings.Contains(segment, "--watch") {
						nextKind, nextEvidence = "waiting", "ci_check_watch"
					}
				}
			}
			if len(fields) > 2 && fields[1] == "api" {
				for _, f := range fields[2:] {
					if strings.HasSuffix(f, "/merge") {
						nextKind, nextEvidence = "merge", "forge_merge_endpoint"
					}
				}
			}
		case "sleep":
			nextKind, nextEvidence = "waiting", "sleep_command"
		case "apply_patch":
			nextKind, nextEvidence = "implementation", "edit_tool"
		}
		if kind != "" && kind != nextKind {
			return "unclassified", "compound_command"
		}
		kind, evidence = nextKind, nextEvidence
	}
	if kind == "" {
		return "unclassified", "tool_lifecycle"
	}
	return kind, evidence
}

// Only known JSON command fields are decoded. Free-form code and arbitrary
// tool inputs remain opaque; keywords inside a script are not executed evidence.
func activityInputCommand(input string) string {
	if !strings.HasPrefix(strings.TrimSpace(input), "{") {
		return input
	}
	var args struct {
		Command  string `json:"command"`
		Cmd      string `json:"cmd"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	if json.Unmarshal([]byte(input), &args) != nil {
		return ""
	}
	if args.Command != "" {
		return args.Command
	}
	if args.Cmd != "" {
		return args.Cmd
	}
	if args.FilePath != "" {
		return args.FilePath
	}
	return args.Path
}

func activityReadsSource(command string, source activityInstruction) bool {
	for _, field := range strings.Fields(command) {
		path := strings.Trim(field, "\"'")
		if path == source.ref.Name || path == "./"+source.ref.Name || (source.path != "" && path == source.path) {
			return true
		}
	}
	return false
}

// Native action type/name/path values may contain private provider data. Only
// known types and content-free hashes leave the recorder.
func activityAction(action NativeCommandAction, index int, sources []activityInstruction, observation activityObservation) workflowmetrics.ActivityAction {
	record := workflowmetrics.ActivityAction{Index: index, Type: "unknown", TypeRef: activityHash(action.Type), Fingerprint: activityHash(observation.update.CWD + "\x00" + action.Type + "\x00" + action.Command + "\x00" + action.Name + "\x00" + action.Path), Attribution: "unattributed", CausalAttribution: "unknown_provider_origin"}
	switch action.Type {
	case "read", "search", "listFiles", "unknown":
		record.Type = action.Type
	}
	if action.Name != "" {
		record.NameRef = activityHash(action.Name)
	}
	if action.Path != "" {
		record.PathRef = activityHash(action.Path)
	}
	if action.Command != "" {
		record.Kind, record.Evidence = classifyActivity("commandExecution", action.Command)
	} else {
		record.Kind, record.Evidence = "unclassified", "opaque_native_action"
	}
	switch record.Type {
	case "read":
		record.Kind, record.Evidence = "context_read", "native_read"
	case "search", "listFiles":
		record.Kind, record.Evidence = "context_read", "native_search"
	}
	if ref, ok := observation.nativeSources[index]; ok {
		record.Sources = append(record.Sources, ref)
		record.Attribution = "observed_read_request"
	}
	record.SourceCoverage = observation.sourceCoverage[index]
	if record.Kind != "context_read" && record.Kind != "unclassified" && record.Kind != "tool_execution" && action.Command != "" {
		for _, source := range sources {
			if line := strings.Index(source.text, action.Command); line >= 0 {
				ref := source.ref
				ref.MatchedLine = 1 + strings.Count(source.text[:line], "\n")
				if len(record.Sources) >= 4 {
					record.SourceCoverage = "inferred_candidates_limited"
					break
				}
				record.Sources = append(record.Sources, ref)
				record.Attribution = "inferred_text_match"
			}
		}
	}
	return record
}

// This runs only on the existing recorder goroutine. A request identifies a
// source candidate; the later local snapshot cannot prove provider read bytes.
func snapshotActivityReads(profile *workflowmetrics.ActivityProfile, sources *[]activityInstruction, snapshots *int, workspace string, observation *activityObservation) {
	for i, action := range observation.update.NativeActions {
		if action.Type != "read" {
			continue
		}
		if observation.sourceCoverage == nil {
			observation.sourceCoverage = make(map[int]string)
		}
		observation.sourceCoverage[i] = "instruction_path_unavailable"
		if action.Path == "" {
			continue
		}
		name := filepath.Base(action.Path)
		if name != "AGENTS.md" && name != "WORKFLOW.md" && name != "CLAUDE.md" {
			observation.sourceCoverage[i] = "not_instruction_file"
			continue
		}
		cwd := observation.update.CWD
		if cwd == "" {
			cwd = workspace
		} else if !filepath.IsAbs(cwd) {
			cwd = filepath.Join(workspace, cwd)
		}
		path := action.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		relative, err := filepath.Rel(workspace, path)
		if err != nil || !filepath.IsLocal(relative) {
			observation.sourceCoverage[i] = "outside_workspace"
			continue
		}
		observation.sourceCoverage[i] = "snapshot_limit"
		if *snapshots >= activitySnapshotLimit || len(profile.Sources) >= activitySourceLimit {
			continue
		}
		*snapshots++
		root, err := os.OpenRoot(workspace)
		if err != nil {
			observation.sourceCoverage[i] = "snapshot_unavailable"
			continue
		}
		// Exclude directories and special files; Root also refuses symlink escapes.
		info, err := root.Stat(relative)
		var data []byte
		if err == nil && info.Mode().IsRegular() {
			var file *os.File
			file, err = root.Open(relative)
			if err == nil {
				data, err = io.ReadAll(io.LimitReader(file, 256*1024+1))
				_ = file.Close() //nolint:errcheck // Read-only telemetry cleanup.
			}
		} else if err == nil {
			err = os.ErrInvalid
		}
		_ = root.Close() //nolint:errcheck // Read-only telemetry cleanup.
		if err != nil || len(data) > 256*1024 {
			observation.sourceCoverage[i] = "snapshot_unavailable"
			continue
		}
		ref := workflowmetrics.InstructionRef{Name: name, PathRef: activityHash(filepath.Clean(relative)), Hash: activityHash(string(data)), ObservedAt: time.Now().UTC(), Evidence: "recorder_snapshot_after_native_read"}
		if observation.nativeSources == nil {
			observation.nativeSources = make(map[int]workflowmetrics.InstructionRef)
		}
		observation.nativeSources[i] = ref
		observation.sourceCoverage[i] = "recorder_snapshot"
		exists := false
		for _, prior := range profile.Sources {
			if prior.PathRef == ref.PathRef && prior.Hash == ref.Hash {
				exists = true
				break
			}
		}
		if !exists {
			profile.Sources = append(profile.Sources, ref)
		}
		next := activityInstruction{ref: ref, text: string(data), path: path}
		replaced := false
		for j, prior := range *sources {
			if prior.path == path {
				(*sources)[j] = next
				replaced = true
				break
			}
		}
		if !replaced {
			*sources = append(*sources, next)
		}
	}
}
