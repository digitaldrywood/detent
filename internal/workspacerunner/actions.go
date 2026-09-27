package workspacerunner

import (
	"context"
	"encoding/base64"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Run-on-worktree-creation (decisions section 18.12).
//
// The hub hands the runner the project's run-on-worktree-creation set with the
// bind, and the runner starts them itself. Nobody asked for these runs: they
// happen because a worktree came into existence, which is why they are reported
// rather than streamed -- there is no stream, and often no person, to send
// output to at the moment they run.

// ActionRun is one report of one project action run.
//
// RunID is empty on a run's first report: the hub creates the row and answers
// with its id, and every later report of the same run carries it. ExitCode is a
// pointer for the reason section 18.12 gives for the stored run's own field --
// "exited 0" and "never exited" are different facts and a zero int cannot hold
// both.
type ActionRun struct {
	ActionID   string
	RunID      string
	Status     string
	ExitCode   *int
	Reason     string
	StartedAt  *time.Time
	FinishedAt *time.Time
	Output     string
	Truncated  bool
}

// ActionRunReporter records project action runs with the hub under the
// workspace lease.
//
// It is a seam of the runner's own rather than a hub client type because it is
// what a test fakes: a session whose only reporting path was a live hub could
// not be tested at all, and the report is the whole visible effect of a run
// nobody watched.
type ActionRunReporter interface {
	ReportActionRun(
		ctx context.Context,
		workspaceID string,
		identity hubclient.WorkspaceIdentity,
		run ActionRun,
	) (workspacesession.Run, error)
}

// maxActionReportBytes is how much of a run's output is reported.
//
// The tail is what is kept, the same choice the workspace hooks make
// (workspace.go's hookOutputTail): when a setup command fails, what explains it
// is the last thing it printed.
const maxActionReportBytes = 16 << 10

// actionReportTimeout bounds one report. A report that cannot be delivered in
// this long is dropped with a warning rather than holding up the worktree the
// person is waiting for.
const actionReportTimeout = 15 * time.Second

// runCreationActions runs the project's run-on-worktree-creation set.
func (s *Session) runCreationActions(ctx context.Context, checkout hubclient.WorkspaceCheckout, actions []workspacesession.Action) {
	if len(actions) == 0 {
		return
	}
	if checkout.Worktree != workspacesession.WorktreeFresh {
		// A retained worktree runs nothing. The setup already ran when this
		// worktree was created, and running it again would redo that work on a
		// tree someone may be reading: reinstalling dependencies under a dev
		// server that is using them, or rebuilding over the output a reader
		// came back to look at.
		s.logger.Debug("workspace.actions_skipped", "worktree", checkout.Worktree, "actions", len(actions))
		return
	}
	// One at a time, in the order the hub sent them, which is the order they
	// were written in. The order is the author's meaning: someone who wants
	// install before build writes install first, and running them concurrently
	// would make that order say nothing -- besides letting two package managers
	// write one tree at the same time.
	for _, action := range actions {
		s.runCreationAction(ctx, action)
	}
}

// runCreationAction runs one action and reports both ends of it.
//
// A run that fails does not fail the workspace. A setup command that exits
// non-zero is information -- it is in the run's row, with its output and its
// code -- and not a reason to deny the reader the worktree they asked for: the
// checkout succeeded, the files are there, and the person is better placed than
// this runner to decide what a failed install means for what they came to read.
func (s *Session) runCreationAction(ctx context.Context, action workspacesession.Action) {
	startedAt := s.config.Now().UTC()
	runID := s.reportActionRun(ctx, ActionRun{
		ActionID: action.ID, Status: workspacesession.RunRunning, StartedAt: &startedAt,
	})
	s.logger.Info("workspace.action_started", "action_id", action.ID, "name", action.Name, "worktree", s.path)

	var output []byte
	truncated := false
	result, err := s.exec.Run(ctx, action.Command, func(span workspacesession.ExecOutput) error {
		if span.Truncated {
			truncated = true
			return nil
		}
		if span.Encoding == "" {
			output = append(output, span.Data...)
			return nil
		}
		decoded, decodeErr := base64.StdEncoding.DecodeString(span.Data)
		if decodeErr != nil {
			// The encoding is this runner's own, so a decode failure here is a
			// bug and not something the command did. The run is worth more than
			// the excerpt, so it continues without this span.
			s.logger.Warn("workspace.action_output_not_decoded", "action_id", action.ID, "error", decodeErr)
			return nil
		}
		output = append(output, decoded...)
		return nil
	})

	finishedAt := s.config.Now().UTC()
	tail, trimmed := actionOutputTail(string(output))
	report := ActionRun{
		ActionID: action.ID, RunID: runID, StartedAt: &startedAt, FinishedAt: &finishedAt,
		Output: tail, Truncated: truncated || trimmed,
	}
	switch {
	case err != nil && ctx.Err() != nil:
		// The workspace is going away mid-run. There is no exit code to report
		// and the reason is what says why, which is section 18.12's rule for a
		// run whose outcome was never established.
		report.Status, report.Reason = workspacesession.RunFailed, workspacesession.RunReasonWorkspaceEnd
	case err != nil:
		report.Status, report.Reason = workspacesession.RunFailed, workspacesession.RunReasonKilled
	default:
		code := result.ExitCode
		report.Status, report.ExitCode = workspacesession.RunStatusForExit(code), &code
		if result.Signal != "" {
			report.Reason = workspacesession.RunReasonKilled
		}
	}
	s.reportActionRun(ctx, report)
	if report.Status == workspacesession.RunFailed {
		s.logger.Warn("workspace.action_failed", "action_id", action.ID, "name", action.Name,
			"exit_code", result.ExitCode, "signal", result.Signal, "reason", report.Reason, "error", err)
		return
	}
	s.logger.Info("workspace.action_succeeded", "action_id", action.ID, "name", action.Name,
		"bytes", result.Bytes, "truncated", report.Truncated)
}

// reportActionRun records one transition and answers with the run's id, which
// on a first report is the id the hub just assigned it.
func (s *Session) reportActionRun(ctx context.Context, run ActionRun) string {
	if s.config.Reporter == nil {
		return run.RunID
	}
	// The report runs on a context of its own for the same reason the unbind
	// does: a cancelled session is exactly when the hub most needs to hear how
	// a run ended, and a report that was skipped leaves a row reading "running"
	// for a process that is gone.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), actionReportTimeout)
	defer cancel()
	recorded, err := s.config.Reporter.ReportActionRun(reportCtx, s.config.WorkspaceID, s.config.Identity, run)
	if err != nil {
		s.logger.Warn("workspace.action_run_not_reported",
			"action_id", run.ActionID, "run_id", run.RunID, "status", run.Status, "error", err)
		return run.RunID
	}
	if recorded.ID != "" {
		return recorded.ID
	}
	return run.RunID
}

// actionOutputTail keeps the last maxActionReportBytes of a run's output and
// reports whether anything was dropped.
func actionOutputTail(output string) (string, bool) {
	if len(output) <= maxActionReportBytes {
		return output, false
	}
	tail := output[len(output)-maxActionReportBytes:]
	// The cut may land inside a rune, so the continuation bytes it left at the
	// front are dropped: an excerpt that starts with half a character is not
	// text, and this one is going to a person to read.
	for len(tail) > 0 && tail[0]&0xC0 == 0x80 {
		tail = tail[1:]
	}
	return tail, true
}
