//go:build darwin || linux

package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type activityBenchmarkStore struct {
	store.Store
	writes atomic.Int64
}

func (s *activityBenchmarkStore) SaveWorkflowActivityProfile(ctx context.Context, id int64, event store.WorkflowPhaseEvent, profile workflowmetrics.ActivityProfile) (int64, error) {
	s.writes.Add(1)
	return s.Store.(activityProfileStore).SaveWorkflowActivityProfile(ctx, id, event, profile)
}

// This controlled experiment uses real short shell tools and long commands,
// received provider lifecycles, SQLite checkpoints, and concurrent audit reads.
// Run with -benchtime=1x -count=6; baseline omits only the recorder.
func BenchmarkActivityProfileWorkloads(b *testing.B) {
	for _, workload := range []struct {
		name               string
		attempts, commands int
		command            string
		queries, native    bool
	}{
		{"short", 1, 120, "printf 'fixture output\\n'", false, false},
		{"long", 1, 1, "sleep 6", false, false},
		{"concurrent", 4, 60, "printf 'fixture output\\n'", false, false},
		{"active_audit", 4, 60, "printf 'fixture output\\n'", true, false},
		{"native_short", 1, 120, "cat AGENTS.md >/dev/null; printf 'fixture output\\n'", false, true},
		{"native_audit", 4, 60, "cat AGENTS.md >/dev/null; printf 'fixture output\\n'", true, true},
	} {
		b.Run(workload.name, func(b *testing.B) {
			for range b.N {
				// Alternate paired order across samples to expose drift and noise.
				order := []bool{false, true}
				if activityBenchmarkOrder.Add(1)%2 == 0 {
					order = []bool{true, false}
				}
				for _, enabled := range order {
					activityBenchmarkWorkload(b, workload.attempts, workload.commands, workload.command, workload.queries, workload.native, enabled)
				}
			}
		})
	}
}

var activityBenchmarkOrder atomic.Uint64

func activityBenchmarkWorkload(b *testing.B, attempts, commands int, command string, queries, native, enabled bool) {
	b.Helper()
	b.StopTimer()
	ctx := b.Context()
	dir := b.TempDir()
	if native {
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Private fixture policy\nRun focused tools\n"), 0600); err != nil {
			b.Fatal(err)
		}
	}
	path := filepath.Join(dir, "profile.db")
	backend, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: path})
	if err != nil {
		b.Fatal(err)
	}
	defer backend.Close()
	measuredStore := &activityBenchmarkStore{Store: backend}
	r := &Runner{store: measuredStore, projectID: "fixture", now: time.Now, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cpuBefore := activityBenchmarkCPU()
	walBefore := activityBenchmarkFileSize(path + "-wal")
	started := time.Now()
	b.StartTimer()
	var workers, auditors sync.WaitGroup
	stopQueries := make(chan struct{})
	var auditCount atomic.Int64
	if queries {
		auditors.Go(func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stopQueries:
					return
				case <-ticker.C:
					timeline, err := backend.IssueWorkflowTimeline(ctx, store.IssueIdentity{ProjectID: "fixture", IssueID: "fixture-issue"})
					if err != nil {
						b.Error(err)
						return
					}
					_ = workflowmetrics.ActivityAudits(timeline.Events, time.Now())
					auditCount.Add(1)
				}
			}
		})
	}
	commandTimes := make([][]time.Duration, attempts)
	eventTimes := make([][]time.Duration, attempts)
	var drains sync.WaitGroup
	for attempt := range attempts {
		workers.Go(func() {
			var recorder *activityRecorder
			if enabled {
				recorder = r.startActivityProfile(ctx, RunRequest{Issue: connector.Issue{ID: "fixture-issue"}, WorkAttemptID: int64(attempt + 1), Generation: 1}, int64(attempt+1), dir, config.Workflow{Prompt: "Run focused tools"}, "implementation")
			}
			progress := newAgentRunProgress(runtimeoutput.Policy{MaxBytes: 4096}, "", "", 0, "", 0)
			emit := func(update AgentUpdate) {
				at := time.Now()
				progress.apply(update, at) // existing runner work is present in both modes
				begin := time.Now()
				recorder.observe(update, at, "fixture-head", started)
				eventTimes[attempt] = append(eventTimes[attempt], time.Since(begin))
			}
			for item := range commands {
				id := fmt.Sprintf("tool-%d", item)
				update := AgentUpdate{Type: AgentUpdateToolStarted, ThreadID: "fixture-thread", TurnID: "fixture-turn", ItemID: id, Tool: "commandExecution", Command: command}
				if native {
					update.CWD = dir
					update.NativeActions = []NativeCommandAction{{Type: "read", Command: "cat AGENTS.md", Name: "AGENTS.md", Path: "AGENTS.md"}, {Type: "unknown", Command: "printf 'fixture output\\n'"}}
				}
				emit(update)
				begin := time.Now()
				cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
				cmd.Dir = dir
				output, err := cmd.Output()
				commandTimes[attempt] = append(commandTimes[attempt], time.Since(begin))
				if err != nil {
					b.Error(err)
				}
				emit(AgentUpdate{Type: AgentUpdateToolOutput, ThreadID: "fixture-thread", TurnID: "fixture-turn", ItemID: id, Tool: "command", Delta: string(output)})
				emit(AgentUpdate{Type: AgentUpdateToolCompleted, ThreadID: "fixture-thread", TurnID: "fixture-turn", ItemID: id, Tool: "commandExecution", Status: "completed", NativeActions: update.NativeActions, CWD: update.CWD})
			}
			emit(AgentUpdate{Type: AgentUpdateTurnCompleted, ThreadID: "fixture-thread", TurnID: "fixture-turn", Status: "completed"})
			recorder.close()
			if recorder != nil {
				drains.Go(func() { <-recorder.done })
			}
		})
	}
	workers.Wait()
	workerElapsed := time.Since(started)
	drains.Wait() // includes deferred persistence CPU without delaying command timings
	close(stopQueries)
	auditors.Wait()
	cpu := activityBenchmarkCPU() - cpuBefore
	b.StopTimer()
	runtime.ReadMemStats(&after)
	var commandSamples, eventSamples []time.Duration
	for i := range attempts {
		commandSamples = append(commandSamples, commandTimes[i]...)
		eventSamples = append(eventSamples, eventTimes[i]...)
	}
	var enqueueTotal time.Duration
	for _, elapsed := range eventSamples {
		enqueueTotal += elapsed
	}
	p95 := func(samples []time.Duration) float64 {
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return float64(samples[(len(samples)*95+99)/100-1].Nanoseconds())
	}
	mode := "off-"
	if enabled {
		mode = "on-"
	}
	report := func(value float64, unit string) { b.ReportMetric(value, mode+unit) }
	report(float64(enqueueTotal.Nanoseconds()), "enqueue-ns")
	report(float64(attempts*commands)/workerElapsed.Seconds(), "commands/s")
	report(float64(workerElapsed.Nanoseconds()), "worker-ns")
	report(p95(commandSamples), "command-p95-ns")
	report(p95(eventSamples), "event-p95-ns")
	report(cpu, "cpu-ns")
	report(float64(after.TotalAlloc-before.TotalAlloc), "allocated-B")
	report(float64(after.Mallocs-before.Mallocs), "allocations")
	report(float64(after.HeapAlloc), "heap-B")
	report(float64(measuredStore.writes.Load()), "writes")
	timeline, err := backend.IssueWorkflowTimeline(ctx, store.IssueIdentity{ProjectID: "fixture", IssueID: "fixture-issue"})
	if err != nil {
		b.Fatal(err)
	}
	var metadataBytes int
	for _, event := range timeline.Events {
		metadataBytes += len(event.MetadataJSON)
	}
	report(float64(metadataBytes), "metadata-B")
	report(float64(max(0, activityBenchmarkFileSize(path+"-wal")-walBefore)), "wal-B")
	report(float64(auditCount.Load()), "audits")
	b.StartTimer()
}

func activityBenchmarkCPU() float64 {
	var total float64
	for _, who := range []int{syscall.RUSAGE_SELF, syscall.RUSAGE_CHILDREN} {
		var usage syscall.Rusage
		if syscall.Getrusage(who, &usage) == nil {
			total += float64(usage.Utime.Sec+usage.Stime.Sec)*1e9 + float64(usage.Utime.Usec+usage.Stime.Usec)*1e3
		}
	}
	return total
}

func activityBenchmarkFileSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}
