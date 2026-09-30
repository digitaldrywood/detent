package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// These are private binary transport modes, not operator CLI commands.
const SSHWorkerArgument = "--internal-ssh-worker"
const SSHProbeArgument = "--internal-ssh-probe"

func sshScratchRoot() string {
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if root := os.Getenv(name); root != "" {
			return root
		}
	}
	return ""
}

func SSHWorkerReady() bool {
	root := sshScratchRoot()
	info, err := os.Stat(root)
	return root != "" && err == nil && info.IsDir()
}

type sshRunner struct {
	*runnerpkg.Runner
	workdir      string
	projectID    string
	memory       globalconfig.Memory
	goBuildSlots int
	connection   serviceapi.Connection
	logger       *slog.Logger
	command      func(context.Context, string, string) *exec.Cmd
	mu           sync.Mutex
	health       map[string]sshHostHealth
}

type sshHostHealth struct {
	available bool
	observed  time.Time
}

type sshBootstrap struct {
	Issues          []connector.Issue
	Version         int
	Workflow        config.Workflow
	Policy          policy.Descriptor
	ProjectID       string
	Workdir         string
	Home            string
	Repository      string
	Memory          globalconfig.Memory
	GoBuildSlots    int
	Connection      serviceapi.Connection
	BudgetChecker   bool
	BudgetEstimator bool
	Run             runnerpkg.SSHRunRequest
}

func sshCommand(ctx context.Context, host, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "-o", "ForwardAgent=no", "--", host, command)
}

func (r *sshRunner) WorkerHostAvailable(ctx context.Context, host string) bool {
	if host == "" || host == "local" {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if health, ok := r.health[host]; ok && time.Since(health.observed) < 15*time.Second {
		return health.available
	}
	probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd := r.command(probeCtx, host, "detent "+SSHProbeArgument)
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	available := err == nil && strings.TrimSpace(string(output)) == fmt.Sprint(runnerpkg.SSHProtocolVersion)
	if ctx.Err() != nil {
		return false
	}
	if r.health == nil {
		r.health = make(map[string]sshHostHealth)
	}
	r.health[host] = sshHostHealth{available: available, observed: time.Now()}
	return available
}

func (r *sshRunner) Run(ctx context.Context, request runnerpkg.RunRequest) (result runnerpkg.RunResult, runErr error) {
	if request.WorkerHost == "" || request.WorkerHost == "local" {
		return r.Runner.Run(ctx, request)
	}
	if request.Execution != nil {
		guarded, stop, err := request.Execution.Guard(ctx)
		if err != nil {
			return result, err
		}
		defer stop()
		ctx = guarded
		defer func() {
			outcome := "succeeded"
			if runErr != nil || result.FinalState != runnerpkg.FinalStateCompleted {
				outcome = "failed"
			}
			if ctx.Err() != nil {
				outcome = "interrupted"
				runErr = errors.Join(runErr, context.Cause(ctx))
			}
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			runErr = errors.Join(runErr, request.Execution.Finish(finishCtx, outcome))
		}()
	}
	var response runnerpkg.SSHRunResponse
	if err := r.callSSH(ctx, request, "run", &response, nil); err != nil {
		return response.Result, err
	}
	if errors.Is(response.Err(), runnerpkg.ErrWorkspacePreparation) {
		return response.Result, r.hostFailure(request.WorkerHost, response.Err())
	}
	return response.Result, response.Err()
}

func (r *sshRunner) callSSH(ctx context.Context, request runnerpkg.RunRequest, method string, response any, issues []connector.Issue) (runErr error) {
	workflow, checker, estimator, err := r.SSHWorkflow(ctx)
	if err != nil {
		return err
	}
	// SSHWorkflow clears the host list for the worker; check authorization from
	// the central runtime snapshot instead of accepting arbitrary destinations.
	if !slices.Contains(r.SSHHosts(), request.WorkerHost) {
		return fmt.Errorf("%w: SSH host is not configured", runnerpkg.ErrWorkspacePreparation)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	source := workflow.Config.Workspace.SourceRoot
	if source == "" {
		source = r.workdir
	}
	bootstrap := sshBootstrap{Issues: issues, Version: runnerpkg.SSHProtocolVersion, Workflow: workflow, Policy: workflow.Config.Policy, ProjectID: r.projectID, Workdir: r.workdir, Home: home, Repository: workspace.RepositoryURL(ctx, source), Memory: r.memory, GoBuildSlots: r.goBuildSlots, Connection: r.connection, BudgetChecker: checker, BudgetEstimator: estimator, Run: runnerpkg.NewSSHRunRequest(request)}
	command := "detent " + SSHWorkerArgument
	cmd := r.command(ctx, request.WorkerHost, command)
	cmd.WaitDelay = time.Second
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return err
	}
	// Stderr may contain provider or hook output. It is never copied into a
	// tracker error or transport diagnostic, which could expose credentials.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		input.Close()
		return r.hostFailure(request.WorkerHost, fmt.Errorf("SSH worker startup: %w", err))
	}
	callbacks := r.SSHRunCallbacks(request)
	defer func() {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		runErr = errors.Join(runErr, callbacks.Finish(finishCtx, time.Now()))
	}()
	peer := runnerpkg.NewSSHPeer(ctx, output, input, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
		if method == "service" {
			return r.forwardService(ctx, args)
		}
		return callbacks.Handle(ctx, method, args)
	})
	err = peer.Call(ctx, method, response, bootstrap)
	input.Close()
	peer.Close()
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if waitErr != nil || errors.Is(err, io.ErrUnexpectedEOF) {
		return r.hostFailure(request.WorkerHost, fmt.Errorf("SSH worker disconnected: %w", errors.Join(err, waitErr)))
	}
	if err != nil {
		return r.hostFailure(request.WorkerHost, err)
	}
	return nil
}

func (r *sshRunner) hostFailure(host string, err error) error {
	r.mu.Lock()
	if r.health == nil {
		r.health = make(map[string]sshHostHealth)
	}
	r.health[host] = sshHostHealth{observed: time.Now()}
	r.mu.Unlock()
	// Consolidate host loss with the existing instance-capacity retry. Its
	// host-specific scope must not suppress the model provider on other hosts.
	return backendcapacity.NewError(backendcapacity.Scope{BackendID: "ssh:" + host, BackendKind: "ssh", Provider: "local"}, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload}, fmt.Errorf("SSH host %s: %w", host, err))
}

// RunSSHWorker serves one run and cancels the existing lifecycle when its SSH
// input closes. It does not open the orchestrator database or start a board.
func RunSSHWorker(ctx context.Context, input io.Reader, output io.Writer, logger *slog.Logger) error {
	ctx, stopSignals := withSSHWorkerSignals(ctx)
	defer stopSignals()
	var peer *runnerpkg.SSHPeer
	ready := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	peer = runnerpkg.NewSSHPeer(ctx, input, output, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
		<-ready
		if (method != "run" && method != "reap" && method != "reconcile") || len(args) != 1 {
			return nil, errors.New("unsupported SSH worker request")
		}
		// A channel carries exactly one worker. Never allow two lifecycles to
		// mutate process environment or share its scratch directory.
		started := false
		once.Do(func() { started = true })
		if !started {
			return nil, errors.New("SSH worker already started")
		}
		defer close(finished)
		var bootstrap sshBootstrap
		if err := json.Unmarshal(args[0], &bootstrap); err != nil {
			return nil, err
		}
		if bootstrap.Version != runnerpkg.SSHProtocolVersion {
			return nil, errors.New("incompatible SSH worker protocol")
		}
		result, err := runSSHBootstrap(ctx, peer, bootstrap, logger, method)
		if method == "run" {
			if err != nil {
				outcome := runnerpkg.RunResult{}
				if response, ok := result.(runnerpkg.SSHRunResponse); ok {
					outcome = response.Result
					err = errors.Join(response.Err(), err)
				}
				return runnerpkg.NewSSHRunResponse(outcome, fmt.Errorf("%w: %w", runnerpkg.ErrWorkspacePreparation, err)), nil
			}
			return result, nil
		}
		return result, err
	})
	close(ready)
	defer peer.Close()
	select {
	case <-finished:
		// The handler sends the result after returning. Keep the output open
		// until the central owner acknowledges it by closing stdin.
		<-peer.Context().Done()
	case <-peer.Context().Done():
		// Join worker teardown before leaving, including its process reaper.
		// If no request arrived there is no lifecycle to join.
		once.Do(func() { close(finished) })
		<-finished
	}
	return nil
}

func runSSHBootstrap(ctx context.Context, peer *runnerpkg.SSHPeer, boot sshBootstrap, logger *slog.Logger, operation string) (result any, runErr error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	boot = mapSSHPaths(boot, home)
	boot.Workflow.Config.Policy = boot.Policy
	scratchRoot := sshScratchRoot()
	if scratchRoot == "" {
		return nil, fmt.Errorf("%w: SSH host must provide TMPDIR, TMP, or TEMP", runnerpkg.ErrWorkspacePreparation)
	}
	scratch, err := os.MkdirTemp(scratchRoot, "worker-")
	if err != nil {
		return nil, err
	}
	goBudget := hostGoBudget(boot.GoBuildSlots)
	// Bootstrap credentials have no durable owner. Remove them after the
	// lifecycle has joined; retained work is never a retained credential.
	defer func() {
		runErr = errors.Join(runErr, os.RemoveAll(filepath.Join(scratch, "github-cli")))
		clean := runErr == nil && operation != "run"
		if response, ok := result.(runnerpkg.SSHRunResponse); ok {
			clean = runErr == nil && response.Err() == nil && response.Result.FinalState == runnerpkg.FinalStateCompleted
		}
		if clean {
			runErr = errors.Join(runErr, os.RemoveAll(scratch))
		}
	}()
	// Provide host-local scratch to the worker before any Go or provider work.
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if err := os.Setenv(name, scratch); err != nil {
			return nil, err
		}
	}
	// Existing lifecycle cleanup owns attempt scratch. Retain it on failures
	// instead of deleting artifacts still owned by escaped descendants.
	if err := runnerpkg.PrepareSSHSource(ctx, boot.Workflow.Config, boot.Workdir, boot.Repository, scratch); err != nil {
		return nil, fmt.Errorf("%w: %w", runnerpkg.ErrWorkspacePreparation, err)
	}
	connection := boot.Connection
	if connection.Address != "" {
		address, closeProxy, err := serveSSHService(peer)
		if err != nil {
			return nil, err
		}
		defer closeProxy()
		connection.Address = address
	}
	deps, err := buildRunnerDependencies(boot.Workflow, boot.ProjectID, boot.Workdir, boot.Memory, runnerpkg.SSHSessionStore{Peer: peer}, logger, connection)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", runnerpkg.ErrWorkspacePreparation, err)
	}
	deps.GoBudget = goBudget
	deps.BudgetGuardBuilder = func(config.Budget) (runnerpkg.BudgetChecker, runnerpkg.DispatchEstimator, error) {
		var checker runnerpkg.BudgetChecker
		var estimator runnerpkg.DispatchEstimator
		if boot.BudgetChecker {
			checker = runnerpkg.SSHBudgetGuard{Peer: peer}
		}
		if boot.BudgetEstimator {
			estimator = runnerpkg.SSHBudgetGuard{Peer: peer}
		}
		return checker, estimator, nil
	}
	run, err := runnerpkg.NewRunner(deps)
	if err != nil {
		return nil, err
	}
	switch operation {
	case "reap":
		return run.ReapWorkspace(ctx, boot.Run.Request.Issue)
	case "reconcile":
		return run.ReconcileWorkspaces(ctx, boot.Issues)
	default:
		outcome, err := run.Run(ctx, boot.Run.Bind(peer))
		return runnerpkg.NewSSHRunResponse(outcome, err), nil
	}
}

func mapSSHPaths(boot sshBootstrap, home string) sshBootstrap {
	mapPath := func(path string) string {
		if path == boot.Home {
			return home
		}
		if strings.HasPrefix(path, boot.Home+string(filepath.Separator)) {
			return filepath.Join(home, strings.TrimPrefix(path, boot.Home+string(filepath.Separator)))
		}
		return path
	}
	boot.Workdir = mapPath(boot.Workdir)
	cfg := &boot.Workflow.Config
	cfg.Workspace.Root = mapPath(cfg.Workspace.Root)
	cfg.Workspace.SourceRoot = mapPath(cfg.Workspace.SourceRoot)
	cfg.Workspace.OutputRoot = mapPath(cfg.Workspace.OutputRoot)
	cfg.Deliverable.OutputRoot = mapPath(cfg.Deliverable.OutputRoot)
	cfg.Budget.PricingPath = mapPath(cfg.Budget.PricingPath)
	if boot.Home != "" {
		cfg.Hooks.AfterCreate = strings.ReplaceAll(cfg.Hooks.AfterCreate, boot.Home+"/", home+"/")
		cfg.Hooks.BeforeRun = strings.ReplaceAll(cfg.Hooks.BeforeRun, boot.Home+"/", home+"/")
		cfg.Hooks.AfterRun = strings.ReplaceAll(cfg.Hooks.AfterRun, boot.Home+"/", home+"/")
		cfg.Hooks.BeforeRemove = strings.ReplaceAll(cfg.Hooks.BeforeRemove, boot.Home+"/", home+"/")
	}
	return boot
}

type sshServiceRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}
type sshServiceResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *sshRunner) forwardService(ctx context.Context, args []json.RawMessage) (any, error) {
	if len(args) != 1 || r.connection.Address == "" {
		return nil, errors.New("SSH service is unavailable")
	}
	var request sshServiceRequest
	if err := json.Unmarshal(args[0], &request); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(request.Path, "/") || strings.HasPrefix(request.Path, "//") {
		return nil, errors.New("invalid SSH service path")
	}
	address := r.connection.Address
	if !strings.Contains(address, "://") {
		address = "http://" + address
	}
	upstream, err := http.NewRequestWithContext(ctx, request.Method, strings.TrimRight(address, "/")+request.Path, bytes.NewReader(request.Body))
	if err != nil {
		return nil, err
	}
	upstream.Header = request.Header
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(upstream)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return sshServiceResponse{Status: response.StatusCode, Header: response.Header, Body: body}, err
}

func serveSSHService(peer *runnerpkg.SSHPeer) (string, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var response sshServiceResponse
		if err := peer.Call(r.Context(), "service", &response, sshServiceRequest{Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header, Body: body}); err != nil {
			http.Error(w, "SSH service unavailable", http.StatusBadGateway)
			return
		}
		for key, values := range response.Header {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Status)
		_, _ = w.Write(response.Body)
	})}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			peer.Close()
		}
	}()
	return listener.Addr().String(), func() { _ = server.Close() }, nil
}
