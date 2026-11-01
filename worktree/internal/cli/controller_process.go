package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/logging"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tui"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

const controllerPipeEnvironment = "DETENT_INTERNAL_CONTROLLER_PIPE"

const controllerProxyKeyHeader = "X-Detent-Control-Key"
const controllerRemoteAddressHeader = "X-Detent-Control-Remote-Address"

const controllerStartupTimeout = time.Minute

type controllerSettings struct {
	Address  string
	Key      string
	Terminal bool
}

type controllerSettingsKey struct{}

type controllerViewKey struct{}

type controllerView struct {
	snapshots *hub.Hub[telemetry.Snapshot]
	program   *tea.Program
}

type controllerReady struct {
	Address string
	Build   buildinfo.Info
}

func (v *controllerView) publish(base telemetry.Snapshot, owners map[*controllerProcess]*telemetry.Snapshot, stopping bool) error {
	base.Running = nil
	for _, snapshot := range owners {
		if snapshot != nil {
			base.Running = append(base.Running, snapshot.Running...)
		}
	}
	slices.SortFunc(base.Running, func(a, b telemetry.Running) int {
		if order := a.StartedAt.Compare(b.StartedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	base.Counts.Running = len(base.Running)
	if stopping {
		base.Shutdown.Status, base.Shutdown.Draining = "draining", true
		base.Shutdown.SessionsRemaining = len(base.Running)
	}
	return v.snapshots.Publish(base)
}

type controllerConnection struct {
	peer        *runner.SSHPeer
	input       *os.File
	output      *os.File
	controller  atomic.Pointer[ShutdownController]
	settings    controllerSettings
	retired     atomic.Bool
	initErr     error
	nextRestart atomic.Uint64
}

type controllerRestart struct {
	ID     uint64
	Binary string
}

func supervisesControllers(cfg BootConfig) bool {
	return cfg.Restart != nil && cfg.Restart.execute != nil && cfg.Restart.control == nil && cfg.Mode == BootModeRunning && cfg.Isolated == nil
}

func ControllerRestartRequest() *RestartRequest {
	raw, present := os.LookupEnv(controllerPipeEnvironment)
	if !present {
		return nil
	}
	connection := &controllerConnection{input: os.NewFile(3, "controller-input"), output: os.NewFile(4, "controller-output")}
	connection.initErr = errors.Join(os.Unsetenv(controllerPipeEnvironment), json.Unmarshal([]byte(raw), &connection.settings))
	connection.peer = runner.NewSSHPeer(context.Background(), connection.input, connection.output, func(_ context.Context, method string, args []json.RawMessage) (any, error) {
		if method != "shutdown" || len(args) != 1 {
			return nil, errors.New("unsupported controller request")
		}
		var request ShutdownRequest
		if err := json.Unmarshal(args[0], &request); err != nil {
			return nil, err
		}
		controller := connection.controller.Load()
		if controller == nil {
			return nil, errors.New("controller startup is incomplete")
		}
		switch request {
		case ShutdownRequestForce:
			controller.RequestForce()
		case ShutdownRequestDrain:
			controller.RequestDrain()
		default:
			return nil, errors.New("unsupported shutdown request")
		}
		return struct{}{}, nil
	})
	return &RestartRequest{control: connection, execute: connection.restart}
}

func (c *controllerConnection) restart(ctx context.Context, binary string) error {
	request := controllerRestart{ID: c.nextRestart.Add(1), Binary: binary}
	done := make(chan error, 1)
	operationCtx := context.WithoutCancel(ctx)
	go func() { done <- c.peer.Call(operationCtx, "restart", nil, request) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		cancelErr := c.peer.Call(operationCtx, "cancel_restart", nil, request.ID)
		restartErr := <-done
		if restartErr == nil {
			return nil
		}
		return errors.Join(restartErr, cancelErr)
	}
}

func (c *controllerConnection) preserveRequestOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		key := request.Header.Get(controllerProxyKeyHeader)
		if c.settings.Key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(c.settings.Key)) == 1 {
			request.RemoteAddr = request.Header.Get(controllerRemoteAddressHeader)
		}
		request.Header.Del(controllerProxyKeyHeader)
		request.Header.Del(controllerRemoteAddressHeader)
		next.ServeHTTP(w, request)
	})
}

func (c *controllerConnection) ready(ctx context.Context, controller *ShutdownController, address string, builds ...buildinfo.Info) error {
	c.controller.Store(controller)
	ready := controllerReady{Address: address}
	if len(builds) > 0 {
		ready.Build = builds[0]
	}
	return c.peer.Call(ctx, "ready", nil, ready)
}

func (c *controllerConnection) publishSnapshots(ctx context.Context, snapshots *hub.Hub[telemetry.Snapshot]) {
	subscription, err := snapshots.Subscribe(ctx)
	if err != nil {
		slog.Warn("subscribe terminal controller snapshots", "error", err)
		return
	}
	defer subscription.Close()
	for snapshot := range subscription.C() {
		if err := c.peer.Call(ctx, "snapshot", nil, snapshot); err != nil {
			if ctx.Err() == nil {
				slog.Warn("forward terminal controller snapshot", "error", err)
			}
			return
		}
	}
}

func (c *controllerConnection) close() error {
	c.peer.Close()
	return errors.Join(c.input.Close(), c.output.Close())
}

type controllerProcess struct {
	peer            *runner.SSHPeer
	cmd             *exec.Cmd
	input           *os.File
	output          *os.File
	cancel          context.CancelFunc
	canceledRestart uint64
}

type controllerEvent struct {
	process   *controllerProcess
	method    string
	value     string
	err       error
	reply     chan error
	build     buildinfo.Info
	snapshot  *telemetry.Snapshot
	restartID uint64
}

func startControllerProcess(ctx context.Context, binary string, args []string, events chan<- controllerEvent, stdout, stderr io.Writer) (*controllerProcess, error) {
	parentInput, childOutput, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	childInput, parentOutput, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, parentInput.Close(), childOutput.Close())
	}
	cmd := detentupdate.ReleaseCommand(ctx, binary, args...)
	procgroup.Configure(ctx, cmd)
	settings, ok := ctx.Value(controllerSettingsKey{}).(controllerSettings)
	if !ok {
		settings = controllerSettings{}
	}
	bootstrap, err := json.Marshal(settings)
	if err != nil {
		return nil, errors.Join(err, parentInput.Close(), parentOutput.Close(), childInput.Close(), childOutput.Close())
	}
	procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: map[string]string{controllerPipeEnvironment: string(bootstrap)}})
	cmd.ExtraFiles, cmd.Stdout, cmd.Stderr = []*os.File{childInput, childOutput}, stdout, stderr
	if err := procgroup.Start(cmd); err != nil {
		return nil, errors.Join(err, parentInput.Close(), parentOutput.Close(), childInput.Close(), childOutput.Close())
	}
	if err := errors.Join(childInput.Close(), childOutput.Close()); err != nil {
		slog.Warn("close controller bootstrap descriptors", "error", err)
	}
	process := &controllerProcess{cmd: cmd, input: parentInput, output: parentOutput}
	process.peer = runner.NewSSHPeer(ctx, parentInput, parentOutput, func(callCtx context.Context, method string, args []json.RawMessage) (any, error) {
		if (method != "restart" && method != "cancel_restart" && method != "ready" && method != "snapshot") || len(args) != 1 {
			return nil, errors.New("unsupported supervisor request")
		}
		reply := make(chan error, 1)
		event := controllerEvent{process: process, method: method, reply: reply}
		switch method {
		case "ready":
			var ready controllerReady
			if err := json.Unmarshal(args[0], &ready); err != nil {
				return nil, err
			}
			event.value, event.build = ready.Address, ready.Build
		case "snapshot":
			event.snapshot = &telemetry.Snapshot{}
			if err := json.Unmarshal(args[0], event.snapshot); err != nil {
				return nil, err
			}
		case "restart":
			var request controllerRestart
			if err := json.Unmarshal(args[0], &request); err != nil {
				return nil, err
			}
			event.value, event.restartID = request.Binary, request.ID
		case "cancel_restart":
			if err := json.Unmarshal(args[0], &event.restartID); err != nil {
				return nil, err
			}
		}
		select {
		case events <- event:
		case <-callCtx.Done():
			return nil, callCtx.Err()
		}
		select {
		case err := <-reply:
			return nil, err
		case <-callCtx.Done():
			return nil, callCtx.Err()
		}
	})
	return process, nil
}

func superviseControllers(ctx context.Context, cfg BootConfig) (runErr error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	path := runtimeStorePath(cfg)
	var lock *instancelock.Lock
	if !runtimeStoreIsMemory(path) {
		var err error
		lock, err = acquireRuntimeInstanceLock(runtimeStoreLockPath(path))
		if err != nil {
			return err
		}
		defer func() { runErr = errors.Join(runErr, lock.Close()) }()
	}
	listener, displayURL, err := listenForBoot(runCtx, cfg)
	if err != nil {
		return err
	}
	listenerOwned := true
	defer func() {
		if listenerOwned {
			runErr = errors.Join(runErr, listener.Close())
		}
	}()
	if lock != nil {
		address, err := dashboardServiceAddress(listener.Addr())
		if err != nil {
			return err
		}
		if err := lock.SetDashboardAddress(address); err != nil {
			return err
		}
	}
	if err := printBootBanner(cfg, displayURL); err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	settings := controllerSettings{Address: listener.Addr().String(), Key: hex.EncodeToString(secret), Terminal: shouldLaunchTerminalDashboard(cfg)}
	runCtx = context.WithValue(runCtx, controllerSettingsKey{}, settings)
	var target atomic.Pointer[url.URL]
	transport := &http.Transport{}
	if defaults, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaults.Clone()
	}
	defer transport.CloseIdleConnections()
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: controllerProxy(settings, &target, transport)}
	serverDone := make(chan error, 1)
	listenerOwned = false
	go func() {
		err := server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel()
		}
		serverDone <- err
	}()
	defer func() {
		runErr = errors.Join(runErr, server.Close())
		if err := <-serverDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("serve controller proxy: %w", err))
		}
	}()
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	var stdout io.Writer = os.Stdout
	var stderr io.Writer = os.Stderr
	if cfg.serviceStdout != nil {
		stdout, stderr = cfg.serviceStdout, cfg.serviceStderr
	}
	if settings.Terminal {
		defer cfg.Shutdown.suppressSignalNotices(true)()
		writer, err := newRotatingLogWriter(runtimeLogPath(cfg), runtimeLogRotation(cfg.Runtime))
		if err != nil {
			return err
		}
		defer func() { runErr = errors.Join(runErr, writer.Close()) }()
		previous := slog.Default()
		level := resolveRuntimeLogLevel(parseSlogLevel(cfg.Runtime.LogLevel.Value), []*slog.LevelVar{cfg.LogLevel})
		slog.SetDefault(slog.New(logging.NewHandler(writer, level, false, logging.SourceFromEnv(os.Getenv))))
		defer slog.SetDefault(previous)
		stdout, stderr = writer, writer
		snapshots := hub.New[telemetry.Snapshot]()
		model, err := tui.NewModel(runCtx, snapshots, tui.WithBuild(cfg.Build), tui.WithLogPath(runtimeLogPath(cfg)), tui.WithInterruptFunc(func() { requestTerminalShutdownInterrupt(cfg.Shutdown) }))
		if err != nil {
			return err
		}
		defer model.Close()
		program := tea.NewProgram(model, terminalDashboardProgramOptions()...)
		runCtx = context.WithValue(runCtx, controllerViewKey{}, &controllerView{snapshots: snapshots, program: program})
		uiDone := make(chan error, 1)
		go func() {
			final, err := runTerminalDashboardProgram(runCtx, program)
			err = errors.Join(err, writeTerminalDashboardSummary(cfg.Output, final, model))
			cancel()
			uiDone <- err
		}()
		defer func() { cancel(); runErr = terminalDashboardError(runErr, <-uiDone) }()
	}
	return runControllerSupervisor(runCtx, cfg.Shutdown, binary, os.Args[1:], stdout, stderr, &target, controllerSupervisorConfig{restart: cfg.Restart})
}

func controllerProxy(settings controllerSettings, target *atomic.Pointer[url.URL], transport http.RoundTripper) http.Handler {
	proxy := &httputil.ReverseProxy{Transport: transport, Rewrite: func(request *httputil.ProxyRequest) {
		if current := target.Load(); current != nil {
			request.SetURL(current)
			request.Out.Host = request.In.Host
			request.Out.Header.Set(controllerProxyKeyHeader, settings.Key)
			request.Out.Header.Set(controllerRemoteAddressHeader, request.In.RemoteAddr)
		}
	}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if target.Load() == nil {
			http.Error(w, "Runner is starting", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

type controllerStarter func(context.Context, string, []string, chan<- controllerEvent, io.Writer, io.Writer) (*controllerProcess, error)

type controllerSupervisorConfig struct {
	start          controllerStarter
	startupContext func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	restart        *RestartRequest
}

func runControllerSupervisor(ctx context.Context, shutdown *ShutdownController, binary string, args []string, stdout, stderr io.Writer, target *atomic.Pointer[url.URL], cfg controllerSupervisorConfig) error {
	if shutdown == nil {
		return errors.New("controller shutdown owner is required")
	}
	defer shutdown.activate()()
	if cfg.start == nil {
		cfg.start = startControllerProcess
	}
	if cfg.startupContext == nil {
		cfg.startupContext = context.WithTimeout
	}
	runCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	events := make(chan controllerEvent)
	processes := make(map[*controllerProcess]*telemetry.Snapshot)
	launch := func(binary string) (*controllerProcess, error) {
		processCtx, stop := context.WithCancel(runCtx)
		process, err := cfg.start(processCtx, binary, args, events, stdout, stderr)
		if err != nil {
			stop()
			return nil, fmt.Errorf("start replacement controller: %w", err)
		}
		process.cancel = stop
		processes[process] = nil
		workers.Go(func() {
			defer stop()
			err := process.cmd.Wait()
			process.peer.Close()
			err = errors.Join(err, process.input.Close(), process.output.Close())
			select {
			case events <- controllerEvent{process: process, method: "exit", err: err}:
			case <-runCtx.Done():
			}
		})
		return process, nil
	}
	starting, err := launch(binary)
	if err != nil {
		return err
	}
	var active *controllerProcess
	var pending *controllerEvent
	var startupCause func() error
	var stopStartup context.CancelFunc
	var startupDone <-chan struct{}
	var ownerDone <-chan struct{}
	ownerError := runCtx.Err
	var startupFailure error
	beginStartup := func() {
		nextCtx, stop := cfg.startupContext(runCtx, controllerStartupTimeout)
		startupCause = func() error { return context.Cause(nextCtx) }
		stopStartup = stop
		startupDone = nextCtx.Done()
		startupFailure = nil
	}
	finishStartup := func() {
		if stopStartup != nil {
			stopStartup()
		}
		startupDone, ownerDone = nil, nil
		ownerError = runCtx.Err
	}
	defer finishStartup()
	beginStartup()
	rejectStartup := func(err error) {
		finishStartup()
		startupFailure = err
		if starting != nil {
			starting.cancel()
		}
	}
	var result error
	stopping := false
	var activeSnapshot telemetry.Snapshot
	publishView := func() error {
		if view, ok := runCtx.Value(controllerViewKey{}).(*controllerView); ok {
			return view.publish(activeSnapshot, processes, stopping)
		}
		return nil
	}
	for len(processes) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-startupDone:
			rejectStartup(fmt.Errorf("controller startup did not complete: %w", startupCause()))
		case <-ownerDone:
			rejectStartup(fmt.Errorf("controller adoption canceled: %w", ownerError()))
		case request := <-shutdown.Requests():
			stopping = true
			if starting != nil {
				rejectStartup(errors.New("controller shutdown requested"))
			}
			if pending != nil {
				pending.reply <- errors.New("controller shutdown requested")
				pending = nil
			}
			for process := range processes {
				workers.Go(func() { process.shutdown(runCtx, request) })
			}
		case event := <-events:
			switch event.method {
			case "snapshot":
				if _, known := processes[event.process]; known {
					processes[event.process] = event.snapshot
					if event.process == active {
						activeSnapshot = *event.snapshot
					}
				}
				event.reply <- publishView()
			case "restart":
				if stopping || event.process != active || starting != nil || strings.TrimSpace(event.value) == "" {
					event.reply <- errors.New("controller replacement is unavailable")
					continue
				}
				if event.restartID == 0 || event.restartID <= event.process.canceledRestart {
					event.reply <- context.Canceled
					continue
				}
				starting, err = launch(event.value)
				if err != nil {
					event.reply <- err
					continue
				}
				pending = &event
				beginStartup()
				owner := event.process.peer.Context()
				ownerDone, ownerError = owner.Done(), owner.Err
			case "cancel_restart":
				event.process.canceledRestart = max(event.process.canceledRestart, event.restartID)
				if pending != nil && pending.process == event.process && pending.restartID == event.restartID {
					rejectStartup(context.Canceled)
				}
				event.reply <- nil
			case "ready":
				if stopping || event.process != starting || startupFailure != nil || startupCause() != nil {
					event.reply <- errors.New("controller startup is no longer current")
					continue
				}
				address, parseErr := url.Parse("http://" + event.value)
				if parseErr != nil || address.Hostname() != defaultWebHost || address.Port() == "" {
					event.reply <- errors.New("controller listener is invalid")
					continue
				}
				target.Store(address)
				finishStartup()
				active, starting = starting, nil
				if view, ok := runCtx.Value(controllerViewKey{}).(*controllerView); ok {
					view.program.Send(event.build)
				}
				event.reply <- nil
				if pending != nil {
					pending.reply <- nil
					pending = nil
				}
			case "exit":
				delete(processes, event.process)
				switch event.process {
				case starting:
					finishStartup()
					starting = nil
					failure := errors.New("replacement controller exited before startup completed")
					if startupFailure != nil {
						failure = startupFailure
					}
					if pending == nil {
						if stopping {
							continue
						}
						return errors.Join(failure, event.err)
					}
					pending.reply <- errors.Join(failure, event.err)
					pending = nil
				case active:
					if !stopping && event.err == nil {
						requestUpdateRestart(shutdown, cfg.restart, event.process.cmd.Path)
					}
					stopping = true
					result = event.err
					for process := range processes {
						workers.Go(func() { process.shutdown(runCtx, ShutdownRequestDrain) })
					}
				}
				if err := publishView(); err != nil {
					slog.Warn("publish terminal controller exit", "error", err)
				}
			}
		}
	}
	return result
}

func (p *controllerProcess) shutdown(ctx context.Context, request ShutdownRequest) {
	if err := p.peer.Call(ctx, "shutdown", nil, request); err != nil && ctx.Err() == nil && p.peer.Context().Err() == nil {
		slog.Warn("request controller shutdown", "pid", p.cmd.Process.Pid, "error", err)
	}
}
