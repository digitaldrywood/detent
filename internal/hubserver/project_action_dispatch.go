package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Handing a queued run to the runner (decisions section 18.12).
//
// A run is a row before it is a process, and until now the only thing that
// turned the row into a process was a person opening the exec channel for it.
// That is right for the reader watching output arrive and wrong for everybody
// else: the eighth dogfood run queued a run through
// POST {nativeBase}/actions/:id/runs, got its 202, and watched the row sit
// queued for three minutes and forty seconds because no browser was attached
// to send the frame. Nothing on the runner polls for a queued run either --
// its lane only runs a project's actions when a worktree is created -- so a
// run requested through the API never ran at all. That is a trap for the API
// and for every headless caller.
//
// The fix is the hub handing the run to the runner it is already holding a
// socket to. The runner needs nothing new for it: the exec channel's run frame
// is the same frame whoever opened the stream, so the run gets the same lease
// validation immediately before the process starts, the same 1 MiB output cap,
// the same kill-on-lease-loss and the same process-group teardown that a
// person's run gets, and the hub records it from the frames it relays exactly
// as it records theirs.
//
// What is new is who opened the stream, and the grace period that decides.

// actionRunDispatchGrace is how long a queued run is left for the person who
// asked for it before the hub hands it out itself.
//
// The window exists because the two executors are not equivalent. Only a
// person's own stream can show them the output as it is produced, so a client
// that queues a run and then opens the exec channel for it -- which is what the
// browser does, in the same turn -- must be the one that starts it. The hub
// steps in for the run nobody came back for. A second is far longer than the
// round trip a client needs and far shorter than any wait a headless caller
// would notice, and the claim below is what makes a run that lands exactly on
// the boundary safe rather than doubled.
const actionRunDispatchGrace = time.Second

// dispatchQueuedActionRuns hands every run still queued past the grace period
// to the runner holding its workspace.
//
// It runs on the maintenance tick rather than on the queueing request, and that
// is the same choice sweepActionRuns makes for the same reason: a run that has
// to wait out a grace period cannot be finished by the request that created it,
// and a timer per run would be one goroutine per row for a deadline the tick
// already visits.
func (w *workspaceService) dispatchQueuedActionRuns(ctx context.Context) {
	now := w.now()
	runs, err := readDispatchableActionRuns(ctx, w.server.database.db,
		now.Add(-actionRunDispatchGrace), actionRunDispatchBatch)
	if err != nil {
		w.logger.Warn("action_run.dispatch_failed", "error", err)
		return
	}
	for _, run := range runs {
		w.dispatchActionRun(ctx, run)
	}
}

// dispatchActionRun claims one queued run for the hub and sends its run frame
// to the workspace's runner.
//
// The order is stream, then claim, then frame, and each step undoes itself when
// the next refuses. Claiming before the stream existed would leave a row
// running with nothing carrying it if the stream could not be opened; sending
// the frame before the claim committed would let the loser of a race start a
// process the hub then refused to record.
func (w *workspaceService) dispatchActionRun(ctx context.Context, run actionRunRecord) {
	runner := w.relay.runnerFor(run.WorkspaceID)
	if runner == nil {
		// Nothing is serving this workspace. The run stays queued and the next
		// tick looks again -- a workspace whose runner never comes back has its
		// runs failed by the sweep when the workspace itself ends, which is the
		// only moment they can be known never to run.
		return
	}
	stream, code := w.relay.openHubStream(run.WorkspaceID, workspacesession.ChannelExec)
	// The pointer is what decides, not the code: guarding on the thing about to
	// be dereferenced is the only guard that stays true if either side grows a
	// case, which is the rule handlePersonFrame states for the same pair.
	if stream == nil {
		if code == "" {
			code = workspacesession.CodeStaleExecution
		}
		w.logger.Info("action_run.not_dispatched", "run_id", run.ID,
			"workspace_id", run.WorkspaceID, "code", code)
		return
	}
	var claimed actionRunRecord
	err := w.server.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		// The row is re-read inside the transaction: the listing that chose it
		// is a snapshot, and the person who queued it may have opened the exec
		// channel for it in between.
		current, err := readProjectActionRunByID(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		// claimed_by holds the stream, so an operator reading the row can find
		// the frames in the relay's own log. It can never collide with a
		// person's connection id, because hubStreamOwner is not one.
		claimed, err = claimActionRun(ctx, tx, current, stream.id, now)
		return err
	})
	if err != nil {
		w.relay.closeStream(ctx, run.WorkspaceID, stream.id)
		if !errors.Is(err, errActionRunClaimed) {
			w.logger.Warn("action_run.not_claimed", "run_id", run.ID,
				"workspace_id", run.WorkspaceID, "error", err)
		}
		return
	}
	payload, err := workspacesession.Encode(workspacesession.ExecRun{
		RunID: claimed.ID, ActionID: claimed.ActionID, Command: claimed.Command})
	if err != nil {
		// The row is already running and nothing will ever carry it, so the
		// stream is closed rather than left open -- closeStream fails the run
		// with what it had, which is nothing, instead of leaving a row that
		// says running for ever.
		w.logger.Warn("action_run.frame_not_encoded", "run_id", claimed.ID, "error", err)
		w.relay.beginExecRun(run.WorkspaceID, stream.id, claimed.ID, claimed.ActionID)
		w.relay.closeStream(ctx, run.WorkspaceID, stream.id)
		return
	}
	// The run is registered before the frame is sent, so the first output span
	// the runner answers with already has a row to accumulate into.
	w.relay.beginExecRun(run.WorkspaceID, stream.id, claimed.ID, claimed.ActionID)
	// No actor is stamped. Section 18.2's stamp says which person asked, and
	// nobody did: this run is the hub acting on a row somebody wrote through
	// the API, and inventing a person for it would put a name on an audit line
	// that names the wrong thing.
	runner.send(workspacesession.Frame{
		Channel: workspacesession.ChannelExec, Stream: stream.id,
		Type: workspacesession.TypeExecRun, Seq: w.relay.nextToRunner(stream), Payload: payload,
	})
	w.logger.Info("action_run.dispatched", "run_id", claimed.ID, "action_id", claimed.ActionID,
		"workspace_id", claimed.WorkspaceID, "stream", stream.id)
	w.notifyRun(claimed)
}
