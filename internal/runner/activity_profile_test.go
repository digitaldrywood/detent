package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestActivityObservationAttributionAndGaps(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct{ name, tool, command, kind, attribution string }{
		{"instruction read", "commandExecution", "cat AGENTS.md", "context_read", "observed_read_request"},
		{"validation", "commandExecution", "go test ./internal/foo", "local_validation", "inferred_text_match"},
		{"json command", "Bash", `{"command":"go test ./internal/foo"}`, "local_validation", "inferred_text_match"},
		{"json read", "Read", `{"file_path":"AGENTS.md"}`, "context_read", "observed_read_request"},
		{"other directory", "Bash", "cat /private/other/AGENTS.md", "context_read", "unattributed"},
		{"file change", "fileChange", "private patch content", "implementation", "unattributed"},
		{"opaque code", "exec", "await tools.exec_command({cmd: 'go test ./internal/foo'})", "unclassified", "unattributed"},
		{"ci observation", "Bash", "gh pr checks 1", "review", "unattributed"},
		{"namespaced wait", "functions.write_stdin", `{"session_id":1}`, "waiting", "unattributed"},
		{"unrecognized name", "credit_lookup", "", "unclassified", "unattributed"},
		{"poll wait", "write_stdin", `{"session_id":1}`, "waiting", "unattributed"},
		{"ci watch", "Bash", "gh pr checks 1 --watch", "waiting", "unattributed"},
		{"edit", "apply_patch", "private patch content", "implementation", "unattributed"},
		{"review", "commandExecution", "git diff", "review", "unattributed"},
		{"rebase", "commandExecution", "git rebase origin/develop", "rebase", "unattributed"},
		{"merge", "commandExecution", "gh api repos/o/r/pulls/1/merge", "merge", "unattributed"},
		{"wait", "commandExecution", "sleep 3", "waiting", "unattributed"},
		{"quoted validation", "commandExecution", "echo 'go test ./internal/foo'", "tool_execution", "unattributed"},
		{"compound", "commandExecution", "cat AGENTS.md; go test ./internal/foo", "unclassified", "unattributed"},
		{"no gate", "commandExecution", "true", "tool_execution", "unattributed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := workflowmetrics.ActivityProfile{StartedAt: at, AsOf: at.Add(time.Minute)}
			open, repeats := map[string]int{}, map[string]int{}
			sources := []activityInstruction{{ref: workflowmetrics.InstructionRef{Name: "AGENTS.md", Hash: activityHash("instructions"), ObservedAt: at}, text: "Run go test ./internal/foo"}}
			for i := range 2 {
				item := []string{"first", "second"}[i]
				command, delta := tt.command, ""
				if strings.HasPrefix(command, "{") {
					command, delta = "", command
				}
				applyActivityObservation(&p, open, repeats, sources, activityObservation{at: at.Add(time.Duration(i*10) * time.Second), head: "exact-head", headAt: at, update: activityUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: item, Tool: tt.tool, Command: command, Delta: delta}})
				applyActivityObservation(&p, open, repeats, sources, activityObservation{at: at.Add(time.Duration(i*10+5) * time.Second), update: activityUpdate{Type: AgentUpdateToolOutput, TurnID: "turn", ItemID: item, Tool: "tool_result", Status: "completed"}})
			}
			if p.Spans[0].Kind != tt.kind || p.Spans[0].Attribution != tt.attribution || p.Spans[1].Repeat != 2 || p.Spans[1].Outcome != "completed" {
				t.Fatalf("spans = %+v", p.Spans)
			}
			applyActivityObservation(&p, open, repeats, sources, activityObservation{at: at, update: activityUpdate{Type: AgentUpdateToolCompleted, ItemID: "missing", TurnID: "turn"}})
			start := activityUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: "locked", Tool: "Bash", Command: "go test ./internal/foo"}
			applyActivityObservation(&p, open, repeats, sources, activityObservation{at: at.Add(20 * time.Second), head: "new-head", headAt: at.Add(20 * time.Second), update: start})
			for _, marker := range []struct {
				delta   string
				seconds int
			}{{"validation_lock", 21}, {"validation_acquired", 24}} {
				applyActivityObservation(&p, open, repeats, sources, activityObservation{at: at.Add(time.Duration(marker.seconds) * time.Second), update: activityUpdate{Type: AgentUpdateToolOutput, ItemID: "locked", TurnID: "turn", Delta: marker.delta}})
			}
			applyActivityObservation(&p, open, repeats, sources, activityObservation{at: at.Add(25 * time.Second), update: activityUpdate{Type: AgentUpdateToolCompleted, ItemID: "locked", TurnID: "turn", Status: "failed"}})
			if p.Spans[2].Repeat != 1 || p.Spans[2].Outcome != "failed" || p.Breakdown().ByKind["waiting"] < 3 {
				t.Fatalf("new head/lock accounting: %+v", p.Spans)
			}
			if p.Unpaired != 1 {
				t.Fatalf("unpaired=%d", p.Unpaired)
			}
			data, _ := json.Marshal(p)
			if (tt.command != "" && strings.Contains(string(data), tt.command)) || strings.Contains(string(data), "private patch") {
				t.Fatalf("private input persisted: %s", data)
			}
		})
	}
}

type activityCheckpointProbe struct {
	SessionStore
	started  chan struct{}
	release  chan struct{}
	profiles chan store.WorkflowPhaseEvent
}

func (p *activityCheckpointProbe) SaveWorkflowActivityProfile(_ context.Context, id int64, event store.WorkflowPhaseEvent, profile workflowmetrics.ActivityProfile) (int64, error) {
	if id == 0 {
		close(p.started)
		<-p.release
	}
	data, err := json.Marshal(profile)
	if err != nil {
		return 0, err
	}
	event.MetadataJSON = string(data)
	p.profiles <- event
	return 1, nil
}

func TestActivityRecorderDoesNotWaitForPersistence(t *testing.T) {
	probe := &activityCheckpointProbe{started: make(chan struct{}), release: make(chan struct{}), profiles: make(chan store.WorkflowPhaseEvent, 2)}
	r := &Runner{store: probe, projectID: "test", now: time.Now, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	recorder := r.startActivityProfile(RunRequest{Issue: connector.Issue{ID: "1"}, WorkAttemptID: 3390, Generation: 23}, 42, t.TempDir(), config.Workflow{Prompt: "Run go test ./foo"}, "implementation")
	if recorder == nil {
		t.Fatal("activity profile did not start for the persistence probe")
	}
	<-probe.started
	recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, ItemID: "large", Command: strings.Repeat("private", 2048)}, time.Now(), "", time.Time{})
	for range 300 {
		recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: "same", Tool: "Bash", Command: "go test ./foo"}, time.Now(), "head", time.Now())
	}
	recorder.close() // returns while the telemetry consumer is blocked in storage
	if recorder.dropped.Load() != 45 {
		t.Fatalf("drops=%d", recorder.dropped.Load())
	}
	close(probe.release)
	<-recorder.done
	<-probe.profiles
	final := <-probe.profiles
	var p workflowmetrics.ActivityProfile
	if err := json.Unmarshal([]byte(final.MetadataJSON), &p); err != nil {
		t.Fatal(err)
	}
	if p.Dropped != 45 || p.Unpaired != 255 || p.Status != "ended_without_terminal_event" || p.Spans[0].Outcome != "unobserved" || p.AttemptID != 3390 || p.SessionID != 42 {
		t.Fatalf("profile=%+v", p)
	}
}

// Catches checkpoints that lose the active timeline, completed categories or
// interrupted tool starts, even though the recorder's final flush succeeds.
func TestActivityRecorderAuditsActiveAndInterruptedRuns(t *testing.T) {
	for _, stage := range []string{"planning", "implementation", "rework", "validation", "merging"} {
		t.Run(stage, func(t *testing.T) {
			at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
			var offset atomic.Int64
			probe := &activityCheckpointProbe{started: make(chan struct{}), release: make(chan struct{}), profiles: make(chan store.WorkflowPhaseEvent, 4)}
			close(probe.release)
			workspace := t.TempDir()
			instructions := "Private policy\nRun go test ./internal/fixture\n"
			if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte(instructions), 0600); err != nil {
				t.Fatal(err)
			}
			r := &Runner{store: probe, projectID: "fixture", now: func() time.Time { return at.Add(time.Duration(offset.Load())) }, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			recorder := r.startActivityProfile(RunRequest{Issue: connector.Issue{ID: "fixture-issue"}, WorkAttemptID: 42, Generation: 2}, 43, workspace, config.Workflow{Prompt: "Run go test ./internal/fixture"}, stage)
			<-probe.profiles // initial durable coverage boundary
			for i, command := range []string{"cat AGENTS.md", "go test ./internal/fixture", "go test ./internal/fixture", "git diff", "git rebase origin/develop", "gh api repos/fixture/repo/pulls/1/merge", "sleep 1"} {
				item := strconv.Itoa(i)
				recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: item, Tool: "Bash", Command: command}, at.Add(time.Duration(i*2+1)*time.Second), "fixture-head", at)
				recorder.observe(AgentUpdate{Type: AgentUpdateToolCompleted, TurnID: "turn", ItemID: item, Status: "completed"}, at.Add(time.Duration(i*2+2)*time.Second), "", time.Time{})
			}
			recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: "edit", Tool: "fileChange"}, at.Add(15*time.Second), "fixture-head", at)
			recorder.observe(AgentUpdate{Type: AgentUpdateToolCompleted, TurnID: "turn", ItemID: "edit", Status: "completed"}, at.Add(16*time.Second), "", time.Time{})
			recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: "lost", Tool: "Bash", Command: "go test ./internal/fixture"}, at.Add(17*time.Second), "fixture-head", at)
			offset.Store(int64(20 * time.Second))
			recorder.wake <- struct{}{} // advance the existing checkpoint without a wall-clock sleep
			active := <-probe.profiles
			audits := workflowmetrics.ActivityAudits([]workflowmetrics.PhaseEvent{active}, at.Add(21*time.Second))
			if len(audits) != 1 || audits[0].Profile.Stage != stage || audits[0].Breakdown.ObservedSeconds != 8 || audits[0].Breakdown.UnknownSeconds != 12 || audits[0].UnobservedTailSeconds != 1 {
				t.Fatalf("active audit=%+v", audits)
			}
			for _, kind := range []string{"context_read", "implementation", "local_validation", "review", "rebase", "merge", "waiting"} {
				if audits[0].Breakdown.ByKind[kind] == 0 {
					t.Fatalf("missing %s: %+v", kind, audits[0])
				}
			}
			if audits[0].Profile.Spans[2].Repeat != 2 || audits[0].Profile.Spans[2].Sources[1].MatchedLine != 2 || audits[0].Profile.Spans[8].PendingSeconds != 3 {
				t.Fatalf("evidence=%+v", audits[0].Profile.Spans)
			}
			recorder.observe(AgentUpdate{Type: AgentUpdateTurnCompleted, TurnID: "turn", Status: "interrupted"}, at.Add(20*time.Second), "", time.Time{})
			recorder.close()
			<-recorder.done
			final := <-probe.profiles
			audits = workflowmetrics.ActivityAudits([]workflowmetrics.PhaseEvent{final}, at.Add(time.Hour))
			if audits[0].Profile.Status != "cancelled" || audits[0].Profile.Spans[8].Outcome != "unobserved" || audits[0].UnobservedTailSeconds != 0 {
				t.Fatalf("final audit=%+v", audits[0])
			}
			if strings.Contains(final.MetadataJSON, instructions) || strings.Contains(final.MetadataJSON, "go test ./internal/fixture") {
				t.Fatal("instruction or command text leaked")
			}
		})
	}
}

// Catches unbounded telemetry growth during event-heavy or malformed turns.
func TestActivityObservationCapacity(t *testing.T) {
	p := workflowmetrics.ActivityProfile{}
	open, repeats := map[string]int{}, map[string]int{}
	for i := range activitySpanLimit + 1 {
		applyActivityObservation(&p, open, repeats, nil, activityObservation{at: time.Unix(int64(i), 0), update: activityUpdate{Type: AgentUpdateToolStarted, ItemID: strconv.Itoa(i), Tool: "Bash", Command: "true"}})
	}
	if len(p.Spans) != activitySpanLimit || len(open) != activitySpanLimit || p.Dropped != 1 {
		t.Fatalf("spans=%d open=%d dropped=%d", len(p.Spans), len(open), p.Dropped)
	}
}
