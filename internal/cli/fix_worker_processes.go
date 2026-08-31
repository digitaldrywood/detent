package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type workerProcessesFixResult struct {
	Processes []workerProcessFixOutcome `json:"processes"`
	DryRun    bool                      `json:"dry_run"`
	Applied   bool                      `json:"applied"`
	Cancelled bool                      `json:"cancelled"`
	Noop      bool                      `json:"noop"`
}

type workerProcessFixOutcome struct {
	SessionID  int64     `json:"session_id"`
	Identifier string    `json:"identifier,omitempty"`
	PID        int       `json:"pid"`
	GroupID    int       `json:"pgid,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	RSSBytes   int64     `json:"rss_bytes"`
	Outcome    string    `json:"outcome,omitempty"`
}

type workerProcessFixStore interface {
	ListActiveWorkerProcesses(context.Context) ([]store.WorkerProcess, error)
	MarkSessionWorkerProcessReaped(context.Context, int64, store.WorkerProcessReap) error
	Close() error
}

func newWorkerProcessesFixCommand(configPath *string, opts options) *cobra.Command {
	var dryRun bool
	var confirmed bool
	cmd := &cobra.Command{
		Use:     "worker-processes",
		Short:   "Reap agent processes without live Detent sessions",
		Example: "detent fix worker-processes --dry-run\n  detent fix worker-processes --yes",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRun && confirmed {
				return WrapValidation(errors.New("--dry-run and --yes cannot be used together"))
			}
			resolution, err := resolveConfigPathResolution(derefString(configPath), opts)
			if err != nil {
				return err
			}
			read := opts.read
			if read == nil {
				read = func(path string) (globalconfig.Config, error) {
					return globalconfig.Read(path)
				}
			}
			global, err := read(resolution.Path)
			if err != nil {
				return err
			}
			global.Path = resolution.Path
			probe, err := probeDoctorHealth(cmd.Context(), doctorLiveBoot(BootConfig{}, &global), doctorDeps{httpDo: opts.httpDo}.withDefaults())
			if err != nil && probe.Health.Mode == "" {
				return fmt.Errorf("inspect live Detent worker processes at %s: %w", probe.URL, err)
			}
			result := newWorkerProcessesFixResult(probe.Health.OrphanedAgentProcesses)
			result.DryRun = dryRun
			result.Noop = len(result.Processes) == 0
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			if dryRun || result.Noop {
				return out.Write(func(writer io.Writer) error {
					return writeWorkerProcessesFixResult(writer, result)
				}, result)
			}
			if !confirmed {
				if !out.IsJSON() {
					if err := writeWorkerProcessesFixResult(cmd.OutOrStdout(), result); err != nil {
						return err
					}
				}
				ok, err := confirmFix(cmd, "Reap these orphaned agent process groups?", "worker process reap")
				if err != nil {
					return err
				}
				if !ok {
					result.Cancelled = true
					if out.IsJSON() {
						return out.Write(nil, result)
					}
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "Worker process reap cancelled; no processes were signaled.")
					return err
				}
			}
			processStore, err := store.Open(cmd.Context(), store.Config{Backend: store.BackendSQLite, Path: runtimeStorePath(BootConfig{Global: global})})
			if err != nil {
				return err
			}
			applyErr := applyWorkerProcessesFix(cmd.Context(), processStore, &result, procgroup.Terminate, time.Now)
			closeErr := processStore.Close()
			if err := errors.Join(applyErr, closeErr); err != nil {
				return err
			}
			result.Applied = true
			return out.Write(func(writer io.Writer) error {
				return writeWorkerProcessesFixResult(writer, result)
			}, result)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show orphaned agent processes without signaling them")
	cmd.Flags().BoolVar(&confirmed, "yes", false, "reap orphaned agent processes without an interactive confirmation")
	return cmd
}

func newWorkerProcessesFixResult(summary telemetry.OrphanedAgentProcesses) workerProcessesFixResult {
	result := workerProcessesFixResult{Processes: make([]workerProcessFixOutcome, 0, len(summary.Processes))}
	for _, process := range summary.Processes {
		result.Processes = append(result.Processes, workerProcessFixOutcome{
			SessionID:  process.SessionID,
			Identifier: process.Identifier,
			PID:        process.PID,
			GroupID:    process.GroupID,
			StartedAt:  process.StartedAt,
			RSSBytes:   process.RSSBytes,
		})
	}
	return result
}

func applyWorkerProcessesFix(
	ctx context.Context,
	processStore workerProcessFixStore,
	result *workerProcessesFixResult,
	reap workerProcessReapFunc,
	now func() time.Time,
) error {
	if result == nil || len(result.Processes) == 0 {
		return nil
	}
	if reap == nil {
		reap = procgroup.Terminate
	}
	if now == nil {
		now = time.Now
	}
	registered, err := processStore.ListActiveWorkerProcesses(ctx)
	if err != nil {
		return fmt.Errorf("list worker processes: %w", err)
	}
	registrations := make(map[int64]store.WorkerProcess, len(registered))
	for _, process := range registered {
		registrations[process.SessionID] = process
	}
	for index := range result.Processes {
		process := &result.Processes[index]
		outcome, err := reap(ctx, procgroup.Identity{PID: process.PID, GroupID: process.GroupID, StartedAt: process.StartedAt}, procgroup.DefaultTerminationGrace)
		if err != nil {
			return fmt.Errorf("reap worker process %d: %w", process.PID, err)
		}
		process.Outcome = string(outcome)
		if err := cleanupWorkerProcessArtifacts(registrations[process.SessionID]); err != nil {
			return fmt.Errorf("clean worker process %d artifacts: %w", process.PID, err)
		}
		if err := processStore.MarkSessionWorkerProcessReaped(ctx, process.SessionID, store.WorkerProcessReap{
			ReapedAt: now().UTC(),
			Outcome:  process.Outcome,
			Reason:   "doctor_reap",
		}); err != nil {
			return fmt.Errorf("record worker process %d reap: %w", process.PID, err)
		}
	}
	return nil
}

func writeWorkerProcessesFixResult(out io.Writer, result workerProcessesFixResult) error {
	if len(result.Processes) == 0 {
		_, err := fmt.Fprintln(out, "No orphaned agent processes found.")
		return err
	}
	for _, process := range result.Processes {
		identity := strings.TrimSpace(process.Identifier)
		if identity == "" {
			identity = fmt.Sprintf("session %d", process.SessionID)
		}
		line := fmt.Sprintf("%s pid=%d pgid=%d rss=%s", identity, process.PID, process.GroupID, formatWorkerProcessBytes(process.RSSBytes))
		if process.Outcome != "" {
			line += " outcome=" + process.Outcome
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	switch {
	case result.DryRun:
		_, err := fmt.Fprintln(out, "Dry run; no processes were signaled.")
		return err
	case result.Applied:
		_, err := fmt.Fprintln(out, "Orphaned agent processes reaped.")
		return err
	default:
		_, err := fmt.Fprintln(out, "Reaping requires confirmation.")
		return err
	}
}
