package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workspace"
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
			if tt.attribution != "unattributed" && (len(p.Spans[0].Sources) != 1 || p.Spans[0].Sources[0].Hash != activityHash("instructions")) {
				t.Fatalf("observed instruction provenance lost: %+v", p.Spans[0].Sources)
			}
			fingerprintCommand := tt.command
			if strings.HasPrefix(fingerprintCommand, "{") {
				fingerprintCommand = activityInputCommand(fingerprintCommand)
			}
			if p.Spans[0].Fingerprint != activityHash(tt.tool+"\x00"+fingerprintCommand) {
				t.Fatalf("native command fingerprint changed: %+v", p.Spans[0])
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

func TestActivityNativeCommandInput(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct{ name, input, kind, attribution string }{
		{"read", "cat AGENTS.md", "context_read", "observed_read_request"},
		{"quoted read", "cat 'AGENTS.md'", "context_read", "observed_read_request"},
		{"validation", "go test ./internal/foo", "local_validation", "inferred_text_match"},
		{"env validation", "env GOMAXPROCS=2 go test ./internal/foo", "local_validation", "unattributed"},
		{"unset env validation", "env -u TMPDIR -u TMP -u TEMP GOMAXPROCS=2 go test ./internal/foo", "local_validation", "unattributed"},
		{"opaque env script", `env -S "go test ./internal/foo"`, "tool_execution", "unattributed"},
		{"review", "git diff", "review", "unattributed"},
		{"wait", "sleep 3", "waiting", "unattributed"},
		{"mixed", "cat AGENTS.md; go test ./internal/foo", "unclassified", "unattributed"},
		{"quoted validation text", "echo 'go test ./internal/foo'", "tool_execution", "unattributed"},
		{"expansion", "$VALIDATION", "tool_execution", "unattributed"},
		{"missing native actions", "", "tool_execution", "unattributed"},
		{"other directory", "cat /private/other/AGENTS.md", "context_read", "unattributed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			command := "/bin/zsh -lc 'native command'"
			profile := workflowmetrics.ActivityProfile{StartedAt: at}
			sources := []activityInstruction{{ref: workflowmetrics.InstructionRef{Name: "AGENTS.md", Hash: activityHash("instructions"), ObservedAt: at}, text: "Run go test ./internal/foo"}}
			applyActivityObservation(&profile, map[string]int{}, map[string]int{}, sources, activityObservation{at: at, update: activityUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: "tool", Tool: "commandExecution", Command: command, Delta: tt.input}})
			span := profile.Spans[0]
			if span.Kind != tt.kind || span.Attribution != tt.attribution || span.Fingerprint != activityHash("commandExecution\x00"+command) {
				t.Fatalf("native activity = %+v", span)
			}
			if tt.attribution != "unattributed" && (len(span.Sources) != 1 || span.Sources[0].Hash != activityHash("instructions")) {
				t.Fatalf("instruction provenance lost: %+v", span.Sources)
			}
			data, _ := json.Marshal(profile)
			if strings.Contains(string(data), command) || (tt.input != "" && strings.Contains(string(data), tt.input)) {
				t.Fatal("private command persisted")
			}
		})
	}

	// Catches discarded path/type evidence, stale nested source versions and
	// compound duration multiplication without executing provider commands.
	for _, tt := range []struct {
		name, path, cwd, coverage                                                 string
		remove, limit, completionOnly, directory, oversized, symlink, sourceLimit bool
	}{
		{name: "completion-only native evidence", path: "nested/AGENTS.md", coverage: "recorder_snapshot", completionOnly: true},
		{name: "nested changed source", path: "nested/AGENTS.md", coverage: "recorder_snapshot"},
		{name: "native cwd", path: "AGENTS.md", cwd: "nested", coverage: "recorder_snapshot"},
		{name: "absolute workspace path", path: "absolute", coverage: "recorder_snapshot"},
		{name: "missing path", coverage: "instruction_path_unavailable"},
		{name: "outside workspace", path: "../AGENTS.md", coverage: "outside_workspace"},
		{name: "missing file", path: "missing/AGENTS.md", coverage: "snapshot_unavailable"},
		{name: "snapshot bound", path: "nested/AGENTS.md", coverage: "snapshot_limit", limit: true},
		{name: "nonregular source", path: "nested/AGENTS.md", coverage: "snapshot_unavailable", directory: true},
		{name: "oversized source", path: "nested/AGENTS.md", coverage: "snapshot_unavailable", oversized: true},
		{name: "symlink escapes workspace", path: "nested/AGENTS.md", coverage: "snapshot_unavailable", symlink: true},
		{name: "source bound", path: "nested/AGENTS.md", coverage: "snapshot_limit", sourceLimit: true},
		{name: "removed source", path: "nested/AGENTS.md", coverage: "snapshot_unavailable", remove: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			nested := filepath.Join(workspace, "nested", "AGENTS.md")
			if err := os.Mkdir(filepath.Dir(nested), 0700); err != nil {
				t.Fatal(err)
			}
			path := tt.path
			if path == "absolute" {
				path = nested
			}
			p := workflowmetrics.ActivityProfile{StartedAt: at, AsOf: at.Add(30 * time.Second)}
			if tt.sourceLimit {
				p.Sources = make([]workflowmetrics.InstructionRef, activitySourceLimit)
			}
			open, repeats := map[string]int{}, map[string]int{}
			sources := []activityInstruction{}
			snapshots := 0
			for i := range 2 {
				text := "private policy version " + strconv.Itoa(i) + "\nRun go test ./internal/fixture"
				if tt.oversized {
					text += strings.Repeat("x", 256*1024)
				}
				if err := os.Remove(nested); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if err := os.WriteFile(nested, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				if tt.directory || tt.symlink {
					if err := os.Remove(nested); err != nil {
						t.Fatal(err)
					}
					if tt.directory {
						if err := os.Mkdir(nested, 0700); err != nil {
							t.Fatal(err)
						}
					} else {
						external := filepath.Join(t.TempDir(), "AGENTS.md")
						if err := os.WriteFile(external, []byte(text), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(external, nested); err != nil {
							t.Fatal(err)
						}
					}
				}
				if tt.remove {
					if err := os.Remove(nested); err != nil {
						t.Fatal(err)
					}
				}
				if tt.limit {
					snapshots = activitySnapshotLimit
				}
				actions := []NativeCommandAction{{Type: "read", Name: "private name", Path: path}, {Type: "unknown", Command: "go test ./internal/fixture"}, {Type: "private provider type"}}
				o := activityObservation{at: at.Add(time.Duration(i*10) * time.Second), head: "head", update: activityUpdate{Type: AgentUpdateToolStarted, ItemID: strconv.Itoa(i), Tool: "commandExecution", Command: "private compound command", NativeActions: actions, CWD: tt.cwd}}
				if tt.completionOnly {
					o.update.NativeActions = nil
				}
				snapshotActivityReads(&p, &sources, &snapshots, workspace, &o)
				applyActivityObservation(&p, open, repeats, sources, o)
				exit := 19
				completed := activityObservation{at: o.at.Add(5 * time.Second), update: activityUpdate{Type: AgentUpdateToolCompleted, ItemID: strconv.Itoa(i), Status: "failed", ExitCode: &exit}}
				if tt.completionOnly {
					completed.update.NativeActions, completed.update.CWD = actions, tt.cwd
					snapshotActivityReads(&p, &sources, &snapshots, workspace, &completed)
				}
				applyActivityObservation(&p, open, repeats, sources, completed)
				span := p.Spans[i]
				if len(span.Actions) != 3 || span.Kind != "unclassified" || span.Outcome != "failed" || *span.ExitCode != 19 || span.CausalAttribution != "unknown_provider_origin" || span.Fingerprint != activityHash("commandExecution\x00private compound command") {
					t.Fatalf("span=%+v", span)
				}
				read := span.Actions[0]
				if read.Type != "read" || read.Kind != "context_read" || read.SourceCoverage != tt.coverage || read.Repeat != i+1 || read.NameRef != activityHash("private name") || span.Actions[2].Evidence != "opaque_native_action" {
					t.Fatalf("actions=%+v", span.Actions)
				}
				if tt.coverage == "recorder_snapshot" {
					if len(read.Sources) != 1 || read.Sources[0].Hash != activityHash(text) || read.Sources[0].PathRef != activityHash(filepath.Join("nested", "AGENTS.md")) || read.Sources[0].Evidence != "recorder_snapshot_after_native_read" {
						t.Fatalf("source=%+v", read.Sources)
					}
					if read.Attribution != "observed_read_request" || span.Actions[1].Attribution != "inferred_text_match" || len(span.Actions[1].Sources) != 1 || span.Actions[1].Sources[0].Hash != activityHash(text) {
						t.Fatalf("attribution=%+v", span.Actions)
					}
				} else if len(read.Sources) != 0 || read.Attribution != "unattributed" {
					t.Fatalf("unavailable source=%+v", read)
				}
			}
			if len(p.Spans) != 2 || p.Breakdown().ObservedSeconds != 10 || p.Breakdown().ConcurrentSeconds != 0 {
				t.Fatalf("duplicate native timing: %+v", p.Breakdown())
			}
			if tt.coverage == "recorder_snapshot" && (len(p.Sources) != 2 || p.Sources[0].Hash == p.Sources[1].Hash) {
				t.Fatalf("versions=%+v", p.Sources)
			}
			data, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"private policy", "private compound command", "private name", "private provider type", "go test ./internal/fixture", workspace, "nested/AGENTS.md"} {
				if strings.Contains(string(data), private) {
					t.Fatalf("private evidence persisted: %q", private)
				}
			}
		})
	}

	// Catches conflated relative operations in different working directories and
	// stale wait reasons after structured actions replace fallback classification.
	for _, tt := range []struct {
		name, command, actionType, actionCommand, kind, waitReason, secondCWD string
		wantRepeat                                                            int
	}{
		{name: "native wait without command", actionType: "unknown", actionCommand: "sleep 1", kind: "waiting", waitReason: "sleep_command", wantRepeat: 2},
		{name: "native read replaces fallback wait", command: "sleep 1", actionType: "read", kind: "context_read", wantRepeat: 2},
		{name: "same action different cwd", command: "private wrapper", actionType: "unknown", actionCommand: "go test ./internal/fixture", kind: "local_validation", secondCWD: "other", wantRepeat: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			profile := workflowmetrics.ActivityProfile{}
			open, repeats := map[string]int{}, map[string]int{}
			for i := range 2 {
				cwd := "nested"
				if i == 1 && tt.secondCWD != "" {
					cwd = tt.secondCWD
				}
				applyActivityObservation(&profile, open, repeats, nil, activityObservation{at: at, head: "head", update: activityUpdate{Type: AgentUpdateToolStarted, ItemID: strconv.Itoa(i), Tool: "commandExecution", Command: tt.command, CWD: cwd, NativeActions: []NativeCommandAction{{Type: tt.actionType, Command: tt.actionCommand, Path: "AGENTS.md"}}}})
			}
			span := profile.Spans[1]
			if span.Kind != tt.kind || span.WaitReason != tt.waitReason || span.Actions[0].Repeat != tt.wantRepeat {
				t.Fatalf("native classification/repeats: %+v", span)
			}
		})
	}
}

type activityCheckpointProbe struct {
	SessionStore
	started      chan struct{}
	release      chan struct{}
	profiles     chan store.WorkflowPhaseEvent
	checkContext func(context.Context)
}

func (p *activityCheckpointProbe) SaveWorkflowActivityProfile(ctx context.Context, id int64, event store.WorkflowPhaseEvent, profile workflowmetrics.ActivityProfile) (int64, error) {
	if id == 0 {
		close(p.started)
		<-p.release
	}
	if p.checkContext != nil {
		p.checkContext(ctx)
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
	type attributionKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), attributionKey{}, "run-attribution"))
	defer cancel()
	var checkpoints atomic.Int64
	probe := &activityCheckpointProbe{started: make(chan struct{}), release: make(chan struct{}), profiles: make(chan store.WorkflowPhaseEvent, 2)}
	probe.checkContext = func(ctx context.Context) {
		checkpoints.Add(1)
		if got := ctx.Value(attributionKey{}); got != "run-attribution" {
			t.Errorf("checkpoint attribution = %v", got)
		}
		if err := ctx.Err(); err != nil {
			t.Errorf("checkpoint canceled with the run: %v", err)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("checkpoint has no bounded deadline")
		}
	}
	r := &Runner{store: probe, projectID: "test", now: time.Now, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	recorder := r.startActivityProfile(ctx, RunRequest{Issue: connector.Issue{ID: "1"}, WorkAttemptID: 3390, Generation: 23}, 42, t.TempDir(), config.Workflow{Prompt: "Run go test ./foo"}, "implementation")
	if recorder == nil {
		t.Fatal("activity profile did not start for the persistence probe")
	}
	<-probe.started
	recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, ItemID: "large", Command: strings.Repeat("private", 2048)}, time.Now(), "", time.Time{})
	for range 300 {
		recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: "same", Tool: "Bash", Command: "go test ./foo"}, time.Now(), "head", time.Now())
	}
	cancel()         // Both pending and final writes must outlive run cancellation.
	recorder.close() // returns while the telemetry consumer is blocked in storage
	if recorder.dropped.Load() != 45 {
		t.Fatalf("drops=%d", recorder.dropped.Load())
	}
	close(probe.release)
	<-recorder.done
	if got := checkpoints.Load(); got != 2 {
		t.Fatalf("checkpoints = %d, want initial and final writes", got)
	}
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

func TestActivityRecorderSkipsUnchangedCheckpoints(t *testing.T) {
	workspacePath := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		probe := &activityCheckpointProbe{started: make(chan struct{}), release: make(chan struct{}), profiles: make(chan store.WorkflowPhaseEvent, 8)}
		close(probe.release)
		execution := &landingRunExecution{}
		r := &Runner{store: probe, projectID: "test", now: time.Now, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		recorder := r.startActivityProfile(t.Context(), RunRequest{Issue: connector.Issue{ID: "1"}, WorkAttemptID: 42, Generation: 2, Execution: execution}, 43, workspacePath, config.Workflow{Prompt: "Run go test ./foo"}, "implementation")
		synctest.Wait()
		for range 720 {
			time.Sleep(5 * time.Second)
			synctest.Wait()
		}
		if len(probe.profiles) != 1 || len(execution.observations) != 1 {
			t.Fatalf("idle writes: local=%d native=%d", len(probe.profiles), len(execution.observations))
		}
		for _, test := range []struct {
			name   string
			update AgentUpdate
			writes int
		}{
			{"tool started", AgentUpdate{Type: AgentUpdateToolStarted, ItemID: "tool", Tool: "Bash", Command: "go test ./foo"}, 2},
			{"unchanged turn", AgentUpdate{Type: AgentUpdateTurnStarted}, 2},
			{"tool completed", AgentUpdate{Type: AgentUpdateToolCompleted, ItemID: "tool", Status: "completed"}, 3},
			{"dropped input", AgentUpdate{Type: AgentUpdateToolStarted, ItemID: strings.Repeat("x", 513)}, 4},
			{"terminal turn", AgentUpdate{Type: AgentUpdateTurnCompleted, Status: "completed"}, 5},
		} {
			recorder.observe(test.update, time.Now(), "", time.Time{})
			time.Sleep(5 * time.Second)
			synctest.Wait()
			if len(probe.profiles) != test.writes || len(execution.observations) != test.writes {
				t.Fatalf("%s writes: local=%d native=%d, want %d", test.name, len(probe.profiles), len(execution.observations), test.writes)
			}
		}
		recorder.finish()
		if len(probe.profiles) != 6 || len(execution.observations) != 6 {
			t.Fatalf("final writes: local=%d native=%d", len(probe.profiles), len(execution.observations))
		}
		var final workflowmetrics.ActivityProfile
		for len(probe.profiles) > 0 {
			if err := json.Unmarshal([]byte((<-probe.profiles).MetadataJSON), &final); err != nil {
				t.Fatal(err)
			}
		}
		if final.Status != "completed" || final.FinishedAt.IsZero() || final.Dropped != 1 || len(final.Spans) != 1 || final.Spans[0].Outcome != "completed" || final.Spans[0].FinishedAt.IsZero() || final.Spans[0].StartedAt.IsZero() {
			t.Fatalf("final evidence=%+v", final)
		}
	})
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
			workspacePath := t.TempDir()
			issue := workspace.Issue{Identifier: "activity-head", PullRequestHeadSHA: "remote-pr-head"}
			var backend workspace.Backend
			var info workspace.Info
			var err error
			physical := stage == "implementation" || stage == "rework"
			if physical {
				backend, err = workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: workspacePath, SourceRoot: initRunnerSourceRepo(t), AutoBranch: true})
			} else {
				backend, err = workspace.NewFilesystem(workspace.FilesystemOptions{Root: workspacePath})
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err = backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			workspacePath = info.Path
			instructions := "Private policy\nRun go test ./internal/fixture\n"
			if err := os.WriteFile(filepath.Join(workspacePath, "AGENTS.md"), []byte(instructions), 0600); err != nil {
				t.Fatal(err)
			}
			r := &Runner{store: probe, projectID: "fixture", now: func() time.Time { return at.Add(time.Duration(offset.Load())) }, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), workspace: backend}
			recorder := r.startActivityProfile(t.Context(), RunRequest{Issue: connector.Issue{ID: "fixture-issue"}, WorkAttemptID: 42, Generation: 2}, 43, workspacePath, config.Workflow{Prompt: "Run go test ./internal/fixture"}, stage)
			<-probe.profiles // initial durable coverage boundary
			progress := &agentRunProgress{}
			heads := make([]string, 9)
			headTimes := make([]time.Time, 9)
			for i, command := range []string{"cat AGENTS.md", "go test ./internal/fixture", "go test ./internal/fixture", "git diff", "git rebase origin/develop", "gh api repos/fixture/repo/pulls/1/merge", "sleep 1"} {
				refreshAt := at.Add(time.Duration(i*2+1) * time.Second)
				if i == 3 && physical {
					runRunnerGit(t, workspacePath, "commit", "--allow-empty", "-m", "change physical head")
				}
				if i == 4 || i == 6 {
					refreshAt = progress.diffStatsCheckedAt.Add(time.Second)
				}
				if i == 5 {
					if physical {
						if err := os.Rename(filepath.Join(workspacePath, ".git"), filepath.Join(workspacePath, "hidden-git")); err != nil {
							t.Fatal(err)
						}
					} else {
						r.workspace = &fakeWorkspaceBackend{diffErr: workspace.ErrMissingWorkspace}
					}
				}
				beforeRead := time.Now()
				checkedAt := progress.diffStatsCheckedAt
				if i == 0 || i >= 3 {
					r.liveDiffStats(t.Context(), info, issue, progress, refreshAt)
				}
				if i == 0 || i == 3 || i == 5 {
					checkedAt = refreshAt
				}
				if !progress.diffStatsCheckedAt.Equal(checkedAt) {
					t.Fatalf("refresh cadence changed: %v, want %v", progress.diffStatsCheckedAt, checkedAt)
				}
				heads[i], headTimes[i] = progress.diffStats.HeadSHA, progress.diffStatsHeadObservedAt
				if physical && (i == 0 || i == 3) {
					want := strings.TrimSpace(runRunnerGit(t, workspacePath, "rev-parse", "HEAD"))
					if heads[i] != want || headTimes[i].Before(beforeRead) || headTimes[i].After(time.Now()) {
						t.Fatalf("physical head=%q at=%v, want %q", heads[i], headTimes[i], want)
					}
				}
				if i >= 4 && (heads[i] != heads[3] || !headTimes[i].Equal(headTimes[3])) {
					t.Fatalf("cached/failed refresh changed authority: %q at %v", heads[i], headTimes[i])
				}
				item := strconv.Itoa(i)
				update := AgentUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: item, Tool: "Bash", Command: command}
				if i == 0 {
					update.Tool = "commandExecution"
					update.CWD = workspacePath
					update.NativeActions = []NativeCommandAction{{Type: "read", Name: "AGENTS.md", Path: "AGENTS.md"}}
				}
				started := update
				if stage == "validation" {
					started.NativeActions = nil // Provider metadata first arrives on completion.
				}
				recorder.observe(started, at.Add(time.Duration(i*2+1)*time.Second), progress.diffStats.HeadSHA, progress.diffStatsHeadObservedAt)
				recorder.observe(AgentUpdate{Type: AgentUpdateToolCompleted, TurnID: "turn", ItemID: item, Status: "completed", NativeActions: update.NativeActions, CWD: update.CWD}, at.Add(time.Duration(i*2+2)*time.Second), "", time.Time{})
			}
			recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: "edit", Tool: "fileChange"}, at.Add(15*time.Second), progress.diffStats.HeadSHA, progress.diffStatsHeadObservedAt)
			recorder.observe(AgentUpdate{Type: AgentUpdateToolCompleted, TurnID: "turn", ItemID: "edit", Status: "completed"}, at.Add(16*time.Second), "", time.Time{})
			recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, TurnID: "turn", ItemID: "lost", Tool: "Bash", Command: "go test ./internal/fixture"}, at.Add(17*time.Second), progress.diffStats.HeadSHA, progress.diffStatsHeadObservedAt)
			for _, i := range []int{7, 8} {
				heads[i], headTimes[i] = progress.diffStats.HeadSHA, progress.diffStatsHeadObservedAt
			}
			if physical && (heads[0] == heads[3] || !headTimes[3].After(headTimes[0])) {
				t.Fatalf("second physical observation did not advance: %v, %v", heads, headTimes)
			}
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
			for i, span := range audits[0].Profile.Spans {
				if span.Head != heads[i] || !span.HeadObservedAt.Equal(headTimes[i]) || span.CausalAttribution != "unknown_provider_origin" {
					t.Fatalf("span lost physical observation: %+v, want %q at %v", span, heads[i], headTimes[i])
				}
				if physical && span.HeadAttribution != "last_observed_workspace_snapshot" {
					t.Fatalf("head attribution = %q", span.HeadAttribution)
				}
				if !physical && (span.Head != "" || !span.HeadObservedAt.IsZero() || span.HeadAttribution != "") {
					t.Fatalf("Filesystem supplied head authority: %+v", span)
				}
			}
			nativeRead := audits[0].Profile.Spans[0].Actions
			if len(nativeRead) != 1 || nativeRead[0].Repeat != 1 || nativeRead[0].SourceCoverage != "recorder_snapshot" || len(nativeRead[0].Sources) != 1 || nativeRead[0].Sources[0].Hash != activityHash(instructions) || nativeRead[0].CausalAttribution != "unknown_provider_origin" || len(audits[0].Profile.CoverageNotes) == 0 {
				t.Fatalf("native read checkpoint=%+v", nativeRead)
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
	recorder := &activityRecorder{queue: make(chan activityObservation, 4), wake: make(chan struct{}, 1)}
	actions := make([]NativeCommandAction, activityActionLimit+1)
	actions[0] = NativeCommandAction{Type: "read", Path: "AGENTS.md"}
	recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, ItemID: "bounded", NativeActions: actions}, time.Now(), "", time.Time{})
	actions[0].Path = "changed"
	o := <-recorder.queue
	if len(o.update.NativeActions) != activityActionLimit || o.update.ActionsDropped != 1 || o.update.NativeActions[0].Path != "AGENTS.md" {
		t.Fatalf("queued actions=%+v", o.update)
	}
	actions[0].Command = strings.Repeat("private", 2048)
	recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, ItemID: "oversized", NativeActions: actions}, time.Now(), "", time.Time{})
	o = <-recorder.queue
	if len(o.update.NativeActions) != 0 || o.update.ActionsDropped != len(actions) {
		t.Fatalf("oversized actions retained: %+v", o.update)
	}
	applyActivityObservation(&p, map[string]int{}, map[string]int{}, nil, o)
}

func TestActivityRecorderRetainsWholeAttemptAndRecentWork(t *testing.T) {
	workspacePath := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspacePath, "AGENTS.md"), []byte("Private policy\nRun go test ./fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		probe := &activityCheckpointProbe{started: make(chan struct{}), release: make(chan struct{}), profiles: make(chan store.WorkflowPhaseEvent, 4)}
		close(probe.release)
		r := &Runner{store: probe, projectID: "fixture", now: time.Now, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		recorder := r.startActivityProfile(t.Context(), RunRequest{Issue: connector.Issue{ID: "long-attempt"}, WorkAttemptID: 42, Generation: 2}, 43, workspacePath, config.Workflow{Prompt: "Run go test ./fixture"}, "implementation")
		synctest.Wait()
		for i := range 1060 {
			tool, command := "Bash", "cat AGENTS.md"
			if i >= 350 {
				tool, command = "fileChange", "private patch"
			}
			if i >= 700 {
				tool, command = "Bash", "go test ./fixture"
			}
			recorder.observe(AgentUpdate{Type: AgentUpdateToolStarted, ThreadID: "thread", TurnID: "turn", ItemID: strconv.Itoa(i), Tool: tool, Command: command}, time.Now(), "", time.Time{})
			time.Sleep(time.Millisecond)
			recorder.observe(AgentUpdate{Type: AgentUpdateToolCompleted, ThreadID: "thread", TurnID: "turn", ItemID: strconv.Itoa(i), Status: "completed"}, time.Now(), "", time.Time{})
			time.Sleep(2 * time.Millisecond)
			if i%32 == 31 {
				synctest.Wait()
			}
		}
		recorder.observe(AgentUpdate{Type: AgentUpdateTurnCompleted, Status: "completed"}, time.Now(), "", time.Time{})
		recorder.finish()
		if len(probe.profiles) != 2 {
			t.Fatalf("whole attempt amplified checkpoint writes: %d", len(probe.profiles))
		}
		<-probe.profiles
		var profile workflowmetrics.ActivityProfile
		if err := json.Unmarshal([]byte((<-probe.profiles).MetadataJSON), &profile); err != nil {
			t.Fatal(err)
		}
		if len(profile.Spans) > activitySpanLimit || profile.DetailOmitted == 0 || profile.Dropped != 0 || profile.Unpaired != 0 || profile.Status != "completed" {
			t.Fatalf("long-attempt capture: detail=%d omitted=%d dropped=%d unpaired=%d status=%s", len(profile.Spans), profile.DetailOmitted, profile.Dropped, profile.Unpaired, profile.Status)
		}
		public := workflowmetrics.PublicActivityProfile(profile)
		raw, err := json.Marshal(public)
		if err != nil || len(raw) > 128*1024 || public.ProjectionOmitted == 0 || public.Dropped != 0 {
			t.Fatalf("bounded projection: bytes=%d omitted=%d dropped=%d err=%v", len(raw), public.ProjectionOmitted, public.Dropped, err)
		}
		if public.Spans[len(public.Spans)-1].ID != activityHash("thread\x00turn\x001059") || len(public.Spans[len(public.Spans)-1].Sources) == 0 {
			t.Fatal("recent instruction-attributed work was lost")
		}
		summary := (&tracker.NativeRuntimeObservation{Activity: &public}).WithoutActivitySpans().Activity
		for _, p := range []workflowmetrics.ActivityProfile{profile, public, *summary} {
			b := p.Breakdown()
			if math.Abs(b.ObservedSeconds-1.06) > 1e-9 || math.Abs(b.UnknownSeconds-2.12) > 1e-9 || math.Abs(b.ByKind["context_read"]-.35) > 1e-9 || math.Abs(b.ByKind["implementation"]-.35) > 1e-9 || math.Abs(b.ByKind["local_validation"]-.36) > 1e-9 {
				t.Fatalf("beginning/middle/end coverage: %+v", b)
			}
		}
		for _, private := range []string{"Private policy", "private patch", "go test ./fixture", workspacePath} {
			if strings.Contains(string(raw), private) {
				t.Fatalf("private instruction/tool data survived: %q", private)
			}
		}
	})
}

// Compare producer overhead with and without retained native metadata. Storage,
// file reads and hashing must stay off this producer path.
func BenchmarkActivityNativeObserve(b *testing.B) {
	for _, count := range []int{0, 2, 32} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			recorder := &activityRecorder{queue: make(chan activityObservation, 256), wake: make(chan struct{}, 1)}
			actions := make([]NativeCommandAction, count)
			for i := range actions {
				actions[i] = NativeCommandAction{Type: "read", Command: "cat private/AGENTS.md", Name: "AGENTS.md", Path: "private/AGENTS.md"}
			}
			update := AgentUpdate{Type: AgentUpdateToolStarted, ItemID: "item", Tool: "commandExecution", Command: "private compound", NativeActions: actions}
			at := time.Now()
			b.ReportAllocs()
			for b.Loop() {
				recorder.observe(update, at, "head", at)
				<-recorder.queue
			}
		})
	}
}

func BenchmarkActivityInstructionSnapshot(b *testing.B) {
	workspace := b.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte(strings.Repeat("private instruction\n", 512)), 0600); err != nil {
		b.Fatal(err)
	}
	sources := []activityInstruction{}
	b.ReportAllocs()
	for b.Loop() {
		profile := workflowmetrics.ActivityProfile{}
		snapshots := 0
		observation := activityObservation{update: activityUpdate{NativeActions: []NativeCommandAction{{Type: "read", Path: "AGENTS.md"}}}}
		snapshotActivityReads(&profile, &sources, &snapshots, workspace, &observation)
	}
}
