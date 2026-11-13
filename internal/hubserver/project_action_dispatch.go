package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Handing a queued run to the runner.
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
	organization := tracker.OrganizationID(w.server.appOrganizationID())
	w.relay.mu.Lock()
	after := w.relay.actionDispatchAfter
	w.relay.mu.Unlock()
	var page []postgres.ActionRunDispatchRow
	err := w.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(postgres.RelayActionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		page, err = owner.DispatchableActionRunPage(ctx, w.now().Add(-actionRunDispatchGrace), after, actionRunDispatchBatch)
		return err
	})
	if err != nil {
		w.logger.Warn("action_run.dispatch_failed", "error", err)
		return
	}
	if len(page) == 0 {
		w.relay.mu.Lock()
		w.relay.actionDispatchAfter = nil
		w.relay.mu.Unlock()
		return
	}
	for _, entry := range page {
		row := entry.ProjectActionRun
		w.relay.mu.Lock()
		w.relay.actionDispatchAfter = &postgres.ActionRunCursor{CreatedAt: row.CreatedAt, ID: row.ID}
		w.relay.mu.Unlock()
		if !entry.WorkspaceReadOnly && (entry.WorkspaceState == workspacesession.StateReady || entry.WorkspaceState == workspacesession.StateIdle) {
			run, err := w.server.actionRunRecord(ctx, row, false)
			if err != nil {
				w.logger.Warn("action_run.dispatch_failed", "error", err)
				continue
			}
			w.dispatchActionRun(ctx, run)
		}
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
	state, command, refusal := w.claimRelayExecRun(ctx, workspaceRecord{OrganizationID: run.OrganizationID, ProjectID: run.ProjectID, ID: run.WorkspaceID}, runner, workspacesession.ExecRun{RunID: run.ID, ActionID: run.ActionID, Command: run.Command}, stream.ID)
	if refusal != "" {
		w.relay.closeStream(ctx, run.WorkspaceID, stream.ID)
		if refusal != workspacesession.CodeAlreadyRunning {
			w.logger.Info("action_run.not_claimed", "run_id", run.ID, "workspace_id", run.WorkspaceID, "code", refusal)
		}
		return
	}
	payload, err := workspacesession.Encode(workspacesession.ExecRun{
		RunID: state.RunID, ActionID: state.ActionID, Command: command})
	if err != nil {
		// The row is already running and nothing will ever carry it, so the
		// stream is closed rather than left open -- closeStream fails the run
		// with what it had, which is nothing, instead of leaving a row that
		// says running for ever.
		w.logger.Warn("action_run.frame_not_encoded", "run_id", state.RunID, "error", err)
		w.failUndeliveredRun(ctx, state, stream)
		return
	}
	// The run is registered before the frame is sent, so the first output span
	// the runner answers with already has a row to accumulate into. The runner
	// that was looked up must still be the one serving the workspace: one that
	// detached or was replaced since has already had its runs taken, and this
	// one would otherwise stay running with nothing to end it.
	if !w.relay.beginExecRun(run.WorkspaceID, stream.ID, state, runner) {
		w.logger.Info("action_run.runner_changed", "run_id", state.RunID, "workspace_id", state.WorkspaceID)
		w.failUndeliveredRun(ctx, state, stream)
		return
	}
	// No actor is stamped. The relay protocol's stamp says which person asked, and
	// nobody did: this run is the hub acting on a row somebody wrote through
	// the API, and inventing a person for it would put a name on an audit line
	// that names the wrong thing.
	runner.Send(workspacesession.Frame{
		Channel: workspacesession.ChannelExec, Stream: stream.ID,
		Type: workspacesession.TypeExecRun, Seq: w.relay.nextToRunner(stream), Payload: payload,
	})
	w.logger.Info("action_run.dispatched", "run_id", state.RunID, "action_id", state.ActionID,
		"workspace_id", state.WorkspaceID, "stream", stream.ID)
}

func (w *workspaceService) failUndeliveredRun(ctx context.Context, state *execRunState, stream *relayStream) {
	w.failExecRuns(ctx, []*execRunState{state}, workspacesession.RunReasonStreamClosed)
	w.relay.closeStream(ctx, state.WorkspaceID, stream.ID)
}
