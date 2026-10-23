package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/procgroup"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func workerProtocolPreflight(ctx context.Context, path string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, path, SSHProbeArgument)
	procgroup.Configure(probeCtx, cmd)
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
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

func (r *sshRunner) runLocalWorkerLaunch(ctx context.Context, request runnerpkg.RunRequest, captured *localWorkerLaunch) (result runnerpkg.RunResult, runErr error) {
	phase := "implementation"
	switch request.Mode {
	case runnerpkg.RunModePlan:
		phase = "planning"
	case runnerpkg.RunModeMerge:
		phase = "merging"
	}
	responseReceived := false
	defer func() {
		if runErr == nil || responseReceived {
			return
		}
		if execution, ok := request.Execution.(runnerpkg.CompletionExecution); ok {
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			failure := tracker.NativeTerminalFailure{Error: "Execution worker startup or transport failed", ErrorClass: "protocol", TurnStartRefused: true, ObservedAt: time.Now().UTC()}
			runErr = errors.Join(runErr, execution.PrepareFinish(finishCtx, "failed", result.FinalMessage, &failure))
		}
	}()
	if runtime, ok := request.Execution.(runnerpkg.RuntimeExecution); ok {
		if err := runtime.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{LocalAttemptID: request.WorkAttemptID, Generation: request.Generation, Phase: phase, HeartbeatAt: time.Now().UTC()}); err != nil {
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
		artifact, err = r.artifacts.AcquireLaunch(ctx, workerProtocolPreflight, func(status detentupdate.Status) error {
			if status.Critical {
				comparison, err := detentupdate.CompareVersions(status.LatestVersion, status.CurrentVersion)
				if err != nil {
					return err
				}
				if comparison > 0 {
					return detentupdate.ErrWorkerCriticalUpdate
				}
			}
			var err error
			workflow, checker, estimator, err = r.SSHWorkflow(ctx)
			if err != nil {
				return err
			}
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
	scratch, err = os.MkdirTemp(scratchRoot, "execution-")
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
	boot := sshBootstrap{Local: true, CommitIdentity: r.commitIdentity, Version: runnerpkg.SSHProtocolVersion, Workflow: workflow, Policy: workflow.Config.Policy,
		ProjectID: r.projectID, Workdir: r.workdir, Home: home, Repository: workspace.RepositoryURL(ctx, r.workdir),
		Memory: r.memory, GoBuildSlots: r.goBuildSlots, Connection: r.connection,
		BudgetChecker: checker, BudgetEstimator: estimator, Run: runnerpkg.NewSSHRunRequest(request)}
	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd, err := artifact.Command(processCtx, SSHWorkerArgument)
	if err != nil {
		cancel()
		return result, err
	}
	if cacheRoot := toolcache.ManagedBuildFromContext(cacheCtx); cacheRoot != "" {
		procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: map[string]string{"GOCACHE": cacheRoot}})
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
		return errors.Join(ownedArtifact.Release(), releaseCache())
	}
	deferred := false
	var owner runnerpkg.WorkerLifetimeExecution
	defer func() {
		if !deferred {
			runErr = errors.Join(runErr, closeWorker(context.WithoutCancel(ctx)))
		} else if !responseReceived {
			runErr = errors.Join(runErr, owner.CloseWorkers(context.WithoutCancel(ctx)))
		}
	}()
	artifact = nil
	if childCloseErr != nil || identityErr != nil {
		return result, r.hostFailure("local", errors.Join(childCloseErr, identityErr))
	}
	if request.DeferExecutionFinish {
		var ok bool
		owner, ok = request.Execution.(runnerpkg.WorkerLifetimeExecution)
		if !ok {
			return result, errors.New("claim owner cannot retain an execution worker")
		}
		captured := &localWorkerLaunch{workflow: workflow, artifact: ownedArtifact, checker: checker, estimator: estimator, selectedAt: selectedAt, attempt: request.WorkAttemptID, generation: request.Generation, issue: request.Issue.ID}
		if err := owner.KeepWorker(closeWorker, func(ctx context.Context, request runnerpkg.RunRequest) (runnerpkg.RunResult, error) {
			return r.runLocalWorkerLaunch(ctx, request, captured)
		}); err != nil {
			return result, err
		}
		deferred = true
	}
	if runtime, ok := request.Execution.(runnerpkg.RuntimeExecution); ok {
		launch := &tracker.NativeWorkerLaunch{ExecutionVersion: ownedArtifact.Status.LatestVersion, ExecutionCommit: ownedArtifact.Status.LatestCommit,
			ExecutionSHA256: ownedArtifact.Status.BinarySHA256, ControlVersion: ownedArtifact.Status.CurrentVersion,
			PolicyID: boot.Policy.ID, PolicyRevision: boot.Policy.SourceRevision, Protocol: runnerpkg.SSHProtocolVersion, SelectedAt: selectedAt,
			PID: identity.PID, GroupID: identity.GroupID, ProcessStart: identity.StartedAt}
		if err := runtime.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{WorkerLaunch: launch, LocalAttemptID: request.WorkAttemptID, Generation: request.Generation, Phase: phase, HeartbeatAt: selectedAt}); err != nil {
			return result, err
		}
	}
	r.logger.Info("execution worker selected", "issue", request.Issue.Identifier, "execution_version", ownedArtifact.Status.LatestVersion,
		"control_version", ownedArtifact.Status.CurrentVersion, "policy_id", boot.Policy.ID, "pid", identity.PID, "pgid", identity.GroupID)
	var response runnerpkg.SSHRunResponse
	err = peer.Call(ctx, "run", &response, boot)
	if err != nil {
		return response.Result, r.hostFailure("local", fmt.Errorf("execution worker disconnected: %w", err))
	}
	responseReceived = true
	var reviewErr *runnerpkg.NativeImplementationReviewError
	reviewFailed := errors.As(response.Err(), &reviewErr)
	if !request.DeferExecutionFinish && request.Execution != nil {
		outcome := "succeeded"
		if response.Err() != nil || response.Result.FinalState != runnerpkg.FinalStateCompleted {
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
	if changes, ok := request.Execution.(runnerpkg.ChangeExecution); ok && (response.Err() == nil || reviewFailed) {
		if change := changes.NativeChange(); change != nil {
			copy := *change
			if reviewFailed {
				copy.ImplementationReviewError = reviewErr.Error()
			}
			response.Result.NativeChange = &copy
		}
	}
	return response.Result, response.Err()
}
