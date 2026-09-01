package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/update"
)

var (
	ErrManualRestart = errors.New("manual Detent instance cannot be restarted by a service manager")
	ErrNotRunning    = errors.New("detent service is expected to be running but is not")
	ErrUnsupported   = errors.New("background service installation is unsupported on this platform")
)

type ManagerName string

const (
	ManagerManual  ManagerName = "foreground/manual"
	ManagerSystemd ManagerName = "systemd"
	ManagerLaunchd ManagerName = "launchd"
)

const ManagerEnvironment = "DETENT_SERVICE_MANAGER"

type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateUnknown  State = "unknown"
)

type Action string

const (
	ActionNeedsInstall  Action = "needs_install"
	ActionInstalled     Action = "installed"
	ActionStarted       Action = "started"
	ActionRestarted     Action = "restarted"
	ActionAlreadyActive Action = "already_active"
)

type ManagerInfo struct {
	Name           ManagerName `json:"name"`
	Scope          string      `json:"scope,omitempty"`
	Unit           string      `json:"unit"`
	DefinitionPath string      `json:"definition_path,omitempty"`
}

type Inspection struct {
	Present   bool      `json:"present"`
	State     State     `json:"state"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

type Definition struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Manager interface {
	Info() ManagerInfo
	Definition() Definition
	Inspect(context.Context) (Inspection, error)
	Install(context.Context) (Definition, error)
	Start(context.Context) error
	Restart(context.Context) error
	WaitStopped(context.Context) error
}

type ManualInspector func(string) (Inspection, error)

type CommandRunner func(context.Context, string, ...string) (string, error)

type Config struct {
	GOOS             string
	HomeDir          string
	UID              string
	Path             string
	BinaryPath       string
	ConfigPath       string
	Arguments        []string
	LockPath         string
	Version          string
	AutoUpdate       string
	DashboardURL     string
	Install          update.InstallInfo
	Manager          Manager
	InspectManual    ManualInspector
	RunCommand       CommandRunner
	Now              func() time.Time
	PollInterval     time.Duration
	SystemUnitPaths  []string
	UserUnitPaths    []string
	UserSystemdPath  string
	LaunchdPlistPath string
}

type StartOptions struct {
	Install bool
	Restart bool
}

type StartResult struct {
	Action     Action      `json:"action"`
	Manager    ManagerInfo `json:"manager"`
	State      State       `json:"state"`
	PID        int         `json:"pid,omitempty"`
	Definition *Definition `json:"definition,omitempty"`
}

type Status struct {
	InstallMethod  update.InstallSource `json:"install_method"`
	ServiceManager ManagerName          `json:"service_manager"`
	ServiceScope   string               `json:"service_scope,omitempty"`
	Service        string               `json:"service"`
	DefinitionPath string               `json:"definition_path,omitempty"`
	Expected       bool                 `json:"expected_running"`
	State          State                `json:"state"`
	PID            int                  `json:"pid,omitempty"`
	StartedAt      time.Time            `json:"started_at,omitempty"`
	UptimeSeconds  int64                `json:"uptime_seconds,omitempty"`
	Version        string               `json:"version"`
	AutoUpdate     string               `json:"auto_update"`
	DashboardURL   string               `json:"dashboard_url"`
	ConfigPath     string               `json:"config_path"`
}

func (s Status) Running() bool {
	return s.State == StateRunning
}

type Controller struct {
	cfg     Config
	manager Manager
}

func New(cfg Config) (*Controller, error) {
	cfg = normalizeConfig(cfg)
	manager := cfg.Manager
	if manager == nil {
		if cfg.GOOS == "darwin" {
			if err := validateLaunchdConfig(cfg); err != nil {
				return nil, err
			}
		}
		manager = newPlatformManager(cfg)
	}
	return &Controller{cfg: cfg, manager: manager}, nil
}

func (c *Controller) Start(ctx context.Context, opts StartOptions) (StartResult, error) {
	inspection, err := c.manager.Inspect(ctx)
	if err != nil {
		return StartResult{}, fmt.Errorf("inspect %s service: %w", c.manager.Info().Name, err)
	}
	if inspection.Present {
		return c.startManaged(ctx, inspection, opts)
	}

	manual, err := c.cfg.InspectManual(c.cfg.LockPath)
	if err != nil {
		return StartResult{}, fmt.Errorf("inspect manual Detent instance: %w", err)
	}
	if active(manual.State) {
		result := StartResult{
			Action:  ActionAlreadyActive,
			Manager: manualManagerInfo(),
			State:   manual.State,
			PID:     manual.PID,
		}
		if opts.Restart {
			return result, ErrManualRestart
		}
		return result, nil
	}
	if c.manager.Info().Name == ManagerManual {
		return StartResult{}, ErrUnsupported
	}
	if !opts.Install {
		definition := c.manager.Definition()
		return StartResult{
			Action:     ActionNeedsInstall,
			Manager:    c.manager.Info(),
			State:      StateStopped,
			Definition: &definition,
		}, nil
	}

	definition, err := c.manager.Install(ctx)
	if err != nil {
		return StartResult{}, fmt.Errorf("install %s service: %w", c.manager.Info().Name, err)
	}
	if err := c.manager.Start(ctx); err != nil {
		return StartResult{}, fmt.Errorf("start %s service: %w", c.manager.Info().Name, err)
	}
	return StartResult{
		Action:     ActionInstalled,
		Manager:    c.manager.Info(),
		State:      StateRunning,
		Definition: &definition,
	}, nil
}

func (c *Controller) Status(ctx context.Context) (Status, error) {
	inspection, err := c.manager.Inspect(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("inspect %s service: %w", c.manager.Info().Name, err)
	}
	info := c.manager.Info()
	expected := inspection.Present
	if !inspection.Present {
		inspection, err = c.cfg.InspectManual(c.cfg.LockPath)
		if err != nil {
			return Status{}, fmt.Errorf("inspect manual Detent instance: %w", err)
		}
		info = manualManagerInfo()
	}

	status := Status{
		InstallMethod:  c.cfg.Install.Source,
		ServiceManager: info.Name,
		ServiceScope:   info.Scope,
		Service:        info.Unit,
		DefinitionPath: info.DefinitionPath,
		Expected:       expected,
		State:          inspection.State,
		PID:            inspection.PID,
		StartedAt:      inspection.StartedAt,
		Version:        c.cfg.Version,
		AutoUpdate:     c.cfg.AutoUpdate,
		DashboardURL:   c.cfg.DashboardURL,
		ConfigPath:     c.cfg.ConfigPath,
	}
	if status.State == "" {
		status.State = StateStopped
	}
	if !status.StartedAt.IsZero() {
		uptime := c.cfg.Now().Sub(status.StartedAt)
		if uptime > 0 {
			status.UptimeSeconds = int64(uptime / time.Second)
		}
	}
	return status, nil
}

func (c *Controller) startManaged(ctx context.Context, inspection Inspection, opts StartOptions) (StartResult, error) {
	result := StartResult{
		Manager: c.manager.Info(),
		State:   inspection.State,
		PID:     inspection.PID,
	}
	if active(inspection.State) {
		if !opts.Restart {
			result.Action = ActionAlreadyActive
			return result, nil
		}
		if err := c.manager.Restart(ctx); err != nil {
			return StartResult{}, fmt.Errorf("restart %s service: %w", c.manager.Info().Name, err)
		}
		result.Action = ActionRestarted
		result.State = StateRunning
		return result, nil
	}
	if inspection.State == StateStopping {
		if err := c.manager.WaitStopped(ctx); err != nil {
			return StartResult{}, fmt.Errorf("wait for %s service to stop: %w", c.manager.Info().Name, err)
		}
	}
	if err := c.manager.Start(ctx); err != nil {
		return StartResult{}, fmt.Errorf("start %s service: %w", c.manager.Info().Name, err)
	}
	result.Action = ActionStarted
	result.State = StateRunning
	return result, nil
}

func active(state State) bool {
	return state == StateRunning || state == StateStarting
}

func normalizeConfig(cfg Config) Config {
	if strings.TrimSpace(cfg.GOOS) == "" {
		cfg.GOOS = runtime.GOOS
	}
	if strings.TrimSpace(cfg.HomeDir) == "" {
		if homeDir, err := os.UserHomeDir(); err == nil {
			cfg.HomeDir = homeDir
		}
	}
	if strings.TrimSpace(cfg.UID) == "" {
		if current, err := user.Current(); err == nil {
			cfg.UID = current.Uid
		}
	}
	if strings.TrimSpace(cfg.Path) == "" {
		cfg.Path = os.Getenv("PATH")
	}
	if strings.TrimSpace(cfg.Version) == "" {
		cfg.Version = "dev"
	}
	if len(cfg.Arguments) == 0 {
		cfg.Arguments = []string{"--config", cfg.ConfigPath, "--headless"}
	}
	if strings.TrimSpace(cfg.AutoUpdate) == "" {
		cfg.AutoUpdate = "disabled"
	}
	if cfg.InspectManual == nil {
		cfg.InspectManual = inspectManual
	}
	if cfg.RunCommand == nil {
		cfg.RunCommand = runCommand
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	return cfg
}

func newPlatformManager(cfg Config) Manager {
	switch cfg.GOOS {
	case "linux":
		return newSystemdManager(cfg)
	case "darwin":
		return newLaunchdManager(cfg)
	default:
		return manualManager{}
	}
}

func manualManagerInfo() ManagerInfo {
	return ManagerInfo{Name: ManagerManual, Unit: string(ManagerManual)}
}

func inspectManual(path string) (Inspection, error) {
	if strings.TrimSpace(path) == "" {
		return Inspection{State: StateStopped}, nil
	}
	status, err := instancelock.Inspect(path)
	if err != nil {
		return Inspection{}, err
	}
	if status.Status != instancelock.StatusHeld {
		return Inspection{State: StateStopped}, nil
	}
	return Inspection{
		State:     StateRunning,
		PID:       status.Owner.PID,
		StartedAt: status.Owner.StartedAt,
	}, nil
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- service backends select the command and pass arguments without a shell.
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(output), ctx.Err()
	}
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return string(output), fmt.Errorf("%w: %s", err, detail)
		}
		return string(output), err
	}
	return string(output), nil
}

func processStartedAt(ctx context.Context, run CommandRunner, pid int) time.Time {
	if pid <= 0 {
		return time.Time{}
	}
	output, err := run(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	if err != nil {
		return time.Time{}
	}
	startedAt, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(output), time.Local)
	if err != nil {
		return time.Time{}
	}
	return startedAt
}

type manualManager struct{}

func (manualManager) Info() ManagerInfo {
	return manualManagerInfo()
}

func (manualManager) Definition() Definition {
	return Definition{}
}

func (manualManager) Inspect(context.Context) (Inspection, error) {
	return Inspection{State: StateStopped}, nil
}

func (manualManager) Install(context.Context) (Definition, error) {
	return Definition{}, ErrUnsupported
}

func (manualManager) Start(context.Context) error {
	return ErrUnsupported
}

func (manualManager) Restart(context.Context) error {
	return ErrUnsupported
}

func (manualManager) WaitStopped(context.Context) error {
	return nil
}
