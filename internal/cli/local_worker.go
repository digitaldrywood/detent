package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/procstart"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func workerProtocolPreflight(ctx context.Context, path string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := detentupdate.ReleaseCommand(probeCtx, path, SSHProbeArgument)
	procgroup.Configure(probeCtx, cmd)
	cmd.WaitDelay = time.Second
	output, err := commandshell.CommandOutput(probeCtx, cmd, false)
	if err != nil {
		return fmt.Errorf("probe worker protocol: %w", err)
	}
	if strings.TrimSpace(string(output)) != strconv.Itoa(runnerpkg.SSHProtocolVersion) {
		return detentupdate.ErrWorkerProtocolIncompatible
	}
	return nil
}

type localWorkerLaunch struct {
	workflow   config.Workflow
	artifact   *detentupdate.WorkerArtifact
	checker    bool
	estimator  bool
	selectedAt time.Time
	attempt    int64
	generation uint64
	issue      string
}

func (r *sshRunner) runLocalWorker(ctx context.Context, request runnerpkg.RunRequest) (runnerpkg.RunResult, error) {
	if owner, ok := request.Execution.(runnerpkg.WorkerLifetimeExecution); ok {
		if launch := owner.WorkerLauncher(); launch != nil {
			return launch(ctx, request)
		}
	}
	return r.runLocalWorkerLaunch(ctx, request, nil)
}

func prepareAcknowledgedWorker(parentCtx, launchCtx context.Context, request *runnerpkg.RunRequest, boot *sshBootstrap, closeWorker func() error) (bool, error) {
	prepare := request.PrepareRun
	request.PrepareRun = nil
	initialWorkspace := request.AttemptWorkspace
	if err := prepare(parentCtx, request); err != nil {
		return false, err
	}
	current, ok := request.Execution.(runnerpkg.AttemptWorkerExecution)
	if !ok {
		return false, runnerpkg.ErrExecutionAuthorityUnavailable
	}
	if current.AttemptWorkspace() != initialWorkspace {
		return true, closeWorker()
	}
	if err := launchCtx.Err(); err != nil {
		return false, context.Cause(launchCtx)
	}
	boot.Run = runnerpkg.NewSSHRunRequest(*request)
	return false, nil
}

func (r *sshRunner) runLocalWorkerLaunch(ctx context.Context, request runnerpkg.RunRequest, captured *localWorkerLaunch) (result runnerpkg.RunResult, runErr error) {
	parentCtx := ctx
	phase := "implementation"
	switch request.Mode {
	case runnerpkg.RunModePlan:
		phase = "planning"
	case runnerpkg.RunModeMerge:
		phase = "merging"
	}
	responseReceived := false
	workerRegistered := false
	defer func() {
		if runErr == nil || responseReceived {
			return
		}
		if execution, ok := request.Execution.(runnerpkg.CompletionExecution); ok {
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			decision := tracker.FailureAttribution{Outcome: "instance", Reason: "execution worker startup or transport failed"}
			failure := tracker.NativeTerminalFailure{
				Error: "Execution worker startup or transport failed", ErrorClass: "protocol", TurnStartRefused: true, ObservedAt: time.Now().UTC(),
				Decision: &decision,
			}
			runErr = runnerpkg.RestoreFailureDecision(runErr, decision)
			outcome := "failed"
			if workerRegistered && request.DeferExecutionFinish {
				outcome = "interrupted"
			}
			runErr = errors.Join(runErr, execution.PrepareFinish(finishCtx, outcome, result.FinalMessage, &failure))
		}
	}()
	workerExecution, fenced := request.Execution.(runnerpkg.AttemptWorkerExecution)
	preparation := tracker.NativeRuntimeObservation{LocalAttemptID: request.WorkAttemptID, Generation: request.Generation, Phase: phase, HeartbeatAt: time.Now().UTC()}
	if fenced {
		workerExecution.NoteAttemptWorkerPreparation(preparation)
	} else if runtime, ok := request.Execution.(runnerpkg.RuntimeExecution); ok {
		if err := runtime.ObserveRuntime(ctx, preparation); err != nil {
			return result, err
		}
	}
	var workflow config.Workflow
	var checker, estimator bool
	var selectedAt time.Time
	var artifact *detentupdate.WorkerArtifact
	var err error
	if captured != nil {
		if captured.attempt != request.WorkAttemptID || captured.generation != request.Generation || captured.issue != request.Issue.ID || captured.workflow.Config.Policy.ID != request.Policy.ID {
			return result, runnerpkg.ErrExecutionAuthorityUnavailable
		}
		workflow, checker, estimator, selectedAt = captured.workflow, captured.checker, captured.estimator, captured.selectedAt
		artifact, err = captured.artifact.Retain()
	} else {
		preflight := detentupdate.BinaryPreflight(workerProtocolPreflight)
		if fenced {
			preflight = nil
		}
		artifact, err = r.artifacts.AcquireLaunch(ctx, preflight, func(status detentupdate.Status) error {
			if status.Critical {
				comparison, err := detentupdate.CompareVersions(status.LatestVersion, status.CurrentVersion)
				if err != nil {
					return err
				}
				if comparison > 0 {
					return detentupdate.ErrWorkerCriticalUpdate
				}
			}
			workflow, checker, estimator = r.WorkerWorkflowSnapshot()
			if request.Policy.ID != "" && request.Policy.ID != workflow.Config.Policy.ID {
				return errors.New("worker policy differs from the claimed policy")
			}
			selectedAt = time.Now().UTC()
			return nil
		})
	}
	if err != nil {
		return result, r.hostFailure("local", fmt.Errorf("select execution worker: %w", err))
	}
	cacheCtx, releaseCache, err := r.buildCache.Use(ctx)
	if err != nil {
		return result, errors.Join(r.hostFailure("local", err), artifact.Release())
	}
	scratchRoot := strings.TrimSpace(workflow.Config.Workspace.Root)
	var scratch string
	defer func() {
		if artifact != nil {
			runErr = errors.Join(runErr, artifact.Release(), releaseCache())
			if scratch != "" {
				runErr = errors.Join(runErr, workspace.CleanupOwnedPath(scratchRoot, scratch))
			}
		}
	}()
	if scratchRoot == "" {
		return result, r.hostFailure("local", errors.New("execution worker requires a configured workspace root"))
	}
	if err := os.MkdirAll(scratchRoot, 0o700); err != nil {
		return result, r.hostFailure("local", err)
	}
	scratchRoot, err = filepath.EvalSymlinks(scratchRoot)
	if err != nil {
		return result, r.hostFailure("local", err)
	}
	if fenced {
		request.AttemptWorkspace = workerExecution.AttemptWorkspace()
		scratch, err = request.AttemptWorkspace.PrepareScratch(scratchRoot)
	} else {
		scratch, err = os.MkdirTemp(scratchRoot, "execution-")
	}
	if err != nil {
		return result, r.hostFailure("local", err)
	}
	if request.Execution != nil {
		guarded, stop, err := request.Execution.Guard(ctx)
		if err != nil {
			return result, err
		}
		defer stop()
		ctx = guarded
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return result, err
	}
	boot := sshBootstrap{Local: true, CommitIdentity: r.commitIdentity, Version: runnerpkg.SSHProtocolVersion, Policy: workflow.Config.Policy,
		ProjectID: r.projectID, Workdir: r.workdir, Home: home,
		Memory: r.memory, GoBuildSlots: r.goBuildSlots, GoJobSlots: int(r.goJobSlots.Load()), Connection: r.connection,
		BudgetChecker: checker, BudgetEstimator: estimator, GoTestReceipts: r.GoTestReceiptsEnabled(), Run: runnerpkg.NewSSHRunRequest(request)}
	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd, err := artifact.Command(processCtx, SSHWorkerArgument)
	if err != nil {
		cancel()
		return result, err
	}
	if variables := toolcache.ManagedEnvironment(cacheCtx); len(variables) > 0 {
		procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: variables})
	}
	procgroup.SetTempDir(cmd, scratch)
	procgroup.Configure(processCtx, cmd)
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return result, err
	}
	output, childOutput, err := os.Pipe()
	if err != nil {
		cancel()
		return result, errors.Join(err, input.Close())
	}
	cmd.Stdout, cmd.Stderr = childOutput, io.Discard
	if err := procgroup.Start(cmd); err != nil {
		cancel()
		return result, r.hostFailure("local", errors.Join(err, input.Close(), output.Close(), childOutput.Close()))
	}
	groupID := procgroup.GroupID(cmd)
	childCloseErr := childOutput.Close()
	identity, identityErr := procgroup.Inspect(cmd)
	callbacks := r.LocalRunCallbacks(request)
	peer := runnerpkg.NewSSHPeer(processCtx, output, input, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
		if method == "service" {
			return r.forwardService(ctx, args)
		}
		return callbacks.Handle(ctx, method, args)
	})
	callbacks.BindExecutionSources(peer, workflow.Config.Workspace.Root)
	exited := make(chan struct{})
	var waitErr error
	var groupErr error
	go func() {
		waitErr = cmd.Wait()
		groupErr = errors.Join(procgroup.Cleanup(groupID), output.Close())
		peer.Close()
		close(exited)
	}()
	var closeOnce sync.Once
	var closeErr error
	var retireWorker func(context.Context) error
	ownedArtifact := artifact
	closeWorker := func(closeCtx context.Context) error {
		closeOnce.Do(func() {
			closeErr = input.Close()
			if errors.Is(closeErr, os.ErrClosed) {
				closeErr = nil
			}
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-exited:
			case <-closeCtx.Done():
				cancel()
				<-exited
			case <-timer.C:
				cancel()
				<-exited
			}
			cancel()
			if waitErr != nil {
				r.logger.Warn("execution worker exited", "issue", request.Issue.Identifier, "error", waitErr)
			}
		})
		if err := callbacks.Finish(closeCtx, time.Now()); err != nil {
			return errors.Join(closeErr, err)
		}
		if groupErr != nil {
			groupErr = procgroup.Cleanup(groupID)
		}
		if err := errors.Join(closeErr, groupErr); err != nil {
			return err
		}
		if err := workspace.CleanupOwnedPath(scratchRoot, scratch); err != nil {
			return err
		}
		if retireWorker != nil {
			retireCtx, cancel := context.WithTimeout(closeCtx, 10*time.Second)
			defer cancel()
			if err := retireWorker(retireCtx); err != nil {
				return err
			}
		}
		return errors.Join(ownedArtifact.Release(), releaseCache())
	}
	var workerOwner runnerpkg.WorkerLifetimeExecution
	workerClosed := false
	defer func() {
		if workerOwner != nil && runErr != nil {
			runErr = errors.Join(runErr, workerOwner.CloseWorkers(context.WithoutCancel(ctx)))
		} else if workerOwner == nil && !workerClosed {
			runErr = errors.Join(runErr, closeWorker(context.WithoutCancel(ctx)))
		}
	}()
	artifact = nil
	if childCloseErr != nil || identityErr != nil {
		return result, r.hostFailure("local", errors.Join(childCloseErr, identityErr))
	}
	launch := &tracker.NativeWorkerLaunch{ExecutionVersion: ownedArtifact.Status.LatestVersion, ExecutionCommit: ownedArtifact.Status.LatestCommit,
		ExecutionSHA256: ownedArtifact.Status.BinarySHA256, ControlVersion: ownedArtifact.Status.CurrentVersion,
		PolicyID: boot.Policy.ID, PolicyRevision: boot.Policy.SourceRevision, Protocol: runnerpkg.SSHProtocolVersion, SelectedAt: selectedAt,
		PID: identity.PID, GroupID: identity.GroupID, ProcessStart: identity.StartedAt}
	observation := tracker.NativeRuntimeObservation{WorkerLaunch: launch, LocalAttemptID: request.WorkAttemptID, Generation: request.Generation, Phase: phase, HeartbeatAt: time.Now().UTC()}
	if fenced {
		startKey, err := procstart.Identity(identity.PID)
		if err != nil {
			return result, r.hostFailure("local", fmt.Errorf("inspect worker birth identity: %w", err))
		}
		if identity.PID > math.MaxInt32 || identity.GroupID > math.MaxInt32 {
			return result, r.hostFailure("local", errors.New("execution worker process identity exceeds Hub bounds"))
		}
		worker := tracker.NativeAttemptWorker{PID: int32(identity.PID), PGID: int32(identity.GroupID), StartedAt: identity.StartedAt.UTC().Truncate(time.Microsecond), StartKey: startKey}
		primary, err := workerExecution.RegisterAttemptWorker(ctx, worker, observation)
		if err != nil {
			return result, fmt.Errorf("register execution worker: %w", err)
		}
		workerRegistered = true
		if primary {
			retireWorker = func(context.Context) error {
				workerExecution.AttemptWorkerExited(worker)
				return nil
			}
		} else {
			retireWorker, err = callbacks.RegisterOverlappingWorker(ctx, request, store.WorkerProcessRegistration{
				WorkerProcessIdentity: store.WorkerProcessIdentity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt, StartKey: worker.StartKey},
				CleanupRoot:           scratchRoot, CleanupPath: scratch,
			})
			if err != nil {
				return result, fmt.Errorf("register overlapping execution worker: %w", err)
			}
		}
	} else if runtime, ok := request.Execution.(runnerpkg.RuntimeExecution); ok {
		if err := runtime.ObserveRuntime(ctx, observation); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, context.Cause(ctx)
	}
	commandCtx := r.WorkerCommandContext(ctx, request)
	if captured == nil {
		workflow, err = runnerpkg.ResolveSSHWorkflow(commandCtx, workflow)
		if err != nil {
			return result, r.hostFailure("local", fmt.Errorf("resolve execution worker workflow: %w", err))
		}
	}
	boot.Workflow = workflow
	boot.Repository = workspace.RepositoryURL(commandCtx, r.workdir)
	if request.PrepareRun != nil {
		closePreparationWorker := func() error {
			if workerClosed {
				return nil
			}
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			err := closeWorker(closeCtx)
			workerClosed = err == nil
			return err
		}
		relaunch, err := prepareAcknowledgedWorker(parentCtx, ctx, &request, &boot, closePreparationWorker)
		if err != nil {
			responseReceived = true
			return result, err
		}
		if relaunch {
			responseReceived = true
			return r.runLocalWorkerLaunch(parentCtx, request, captured)
		}
	}
	if request.DeferExecutionFinish {
		owner, ok := request.Execution.(runnerpkg.WorkerLifetimeExecution)
		if !ok {
			return result, errors.New("claim owner cannot retain an execution worker")
		}
		captured := &localWorkerLaunch{workflow: workflow, artifact: ownedArtifact, checker: checker, estimator: estimator, selectedAt: selectedAt, attempt: request.WorkAttemptID, generation: request.Generation, issue: request.Issue.ID}
		if err := owner.KeepWorker(closeWorker, func(ctx context.Context, request runnerpkg.RunRequest) (runnerpkg.RunResult, error) {
			return r.runLocalWorkerLaunch(ctx, request, captured)
		}); err != nil {
			return result, err
		}
		workerOwner = owner
	}
	r.logger.Info("execution worker selected", "issue", request.Issue.Identifier, "execution_version", ownedArtifact.Status.LatestVersion,
		"control_version", ownedArtifact.Status.CurrentVersion, "policy_id", boot.Policy.ID, "pid", identity.PID, "pgid", identity.GroupID)
	if err := ctx.Err(); err != nil {
		return result, context.Cause(ctx)
	}
	var response runnerpkg.SSHRunResponse
	err = peer.Call(ctx, "run", &response, boot)
	if err != nil {
		return response.Result, r.hostFailure("local", fmt.Errorf("execution worker disconnected: %w", err))
	}
	responseErr := response.Err()
	responseReceived = !response.Result.TurnStartRefused || !errors.Is(responseErr, runnerpkg.ErrWorkspacePreparation)
	if runnerpkg.StopProviderSupervision(request.Execution, responseErr) {
		return response.Result, responseErr
	}
	var reviewErr *runnerpkg.NativeImplementationReviewError
	reviewFailed := errors.As(responseErr, &reviewErr)
	if !request.DeferExecutionFinish && request.Execution != nil {
		outcome := "succeeded"
		if responseErr != nil || response.Result.FinalState != runnerpkg.FinalStateCompleted {
			outcome = "failed"
		}
		if reviewFailed {
			outcome = "interrupted"
		}
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer finishCancel()
		if err := request.Execution.Finish(finishCtx, outcome); err != nil {
			return response.Result, err
		}
	}
	if changes, ok := request.Execution.(runnerpkg.ChangeExecution); ok && (responseErr == nil || reviewFailed) {
		if change := changes.NativeChange(); change != nil {
			copy := *change
			if reviewFailed {
				copy.ImplementationReviewError = reviewErr.Error()
			}
			response.Result.NativeChange = &copy
		}
	}
	return response.Result, responseErr
}
