package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
)

var (
	ErrConfigExists    = errors.New("global config already exists")
	ErrProjectExists   = errors.New("project already exists")
	ErrProjectNotFound = errors.New("project not found")
)

type Operation string

const (
	OperationInit           Operation = "init"
	OperationAddProject     Operation = "add-project"
	OperationPauseProject   Operation = "pause"
	OperationUnpauseProject Operation = "unpause"
	OperationPromoteProject Operation = "promote"
	OperationRemoveProject  Operation = "remove-project"
)

type Signal struct {
	Operation Operation
	ProjectID string
	Project   globalconfig.Project
}

type BootMode string

const (
	BootModeRunning    BootMode = "running"
	BootModeOnboarding BootMode = "onboarding"
)

type BootConfig struct {
	Mode           BootMode
	Global         globalconfig.Config
	ConfigPathRule globalconfig.PathRule
	WorkflowPath   string
	Host           string
	Port           *int
	Version        string
	Headless       bool
	StdoutTTY      bool
	Output         io.Writer
}

type BootFunc func(context.Context, BootConfig) error

type SignalFunc func(context.Context, Signal) error

type ProjectManager interface {
	Add(context.Context, globalconfig.Project) error
	Remove(context.Context, project.ProjectID) error
	Pause(context.Context, project.ProjectID) error
	Unpause(context.Context, project.ProjectID) error
}

type Option func(*options)

type options struct {
	resolvePath   func(string) (globalconfig.PathResolution, error)
	read          func(string) (globalconfig.Config, error)
	readOrDefault func(string) (globalconfig.Config, error)
	write         func(string, globalconfig.Config) error
	boot          BootFunc
	signal        SignalFunc
	version       string
	stdoutTTY     func() bool
}

func WithBootFunc(boot BootFunc) Option {
	return func(opts *options) {
		if boot != nil {
			opts.boot = boot
		}
	}
}

func WithSignalFunc(signal SignalFunc) Option {
	return func(opts *options) {
		if signal != nil {
			opts.signal = signal
		}
	}
}

func WithVersion(version string) Option {
	return func(opts *options) {
		opts.version = strings.TrimSpace(version)
	}
}

func WithProjectManager(manager ProjectManager) Option {
	return func(opts *options) {
		opts.signal = ProjectManagerSignalFunc(manager)
	}
}

func WithStdoutTTY(stdoutTTY func() bool) Option {
	return func(opts *options) {
		if stdoutTTY != nil {
			opts.stdoutTTY = stdoutTTY
		}
	}
}

func ProjectManagerSignalFunc(manager ProjectManager) SignalFunc {
	if manager == nil {
		return noSignal
	}

	return func(ctx context.Context, signal Signal) error {
		switch signal.Operation {
		case OperationAddProject:
			return manager.Add(ctx, signal.Project)
		case OperationRemoveProject:
			return manager.Remove(ctx, project.ProjectID(signal.ProjectID))
		case OperationPauseProject:
			return manager.Pause(ctx, project.ProjectID(signal.ProjectID))
		case OperationUnpauseProject:
			return manager.Unpause(ctx, project.ProjectID(signal.ProjectID))
		default:
			return nil
		}
	}
}

func NewRootCommand(ctx context.Context, optFns ...Option) *cobra.Command {
	opts := defaultOptions()
	for _, opt := range optFns {
		opt(&opts)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var configPath string
	var host string
	var port int
	var headless bool
	cmd := &cobra.Command{
		Use:          "detent",
		Short:        "Detent agent orchestrator",
		Long:         "Detent is an agent orchestrator for tracker-backed work queues.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			boot, err := resolveBootConfig(configPath, host, port, opts)
			if err != nil {
				return err
			}
			boot.Headless = headless
			boot.StdoutTTY = opts.stdoutTTY()
			boot.Output = cmd.OutOrStdout()
			slog.Info("resolved global config", "path", boot.Global.Path, "rule", boot.ConfigPathRule)
			return opts.boot(cmd.Context(), boot)
		},
	}
	cmd.SetContext(ctx)
	cmd.PersistentFlags().StringVar(&configPath, "config", "", "path to global.yaml")
	cmd.PersistentFlags().StringVar(&host, "host", "", "web server host")
	cmd.PersistentFlags().IntVar(&port, "port", -1, "web server port, or 0 for an ephemeral port")
	cmd.PersistentFlags().BoolVar(&headless, "headless", false, "stream logs instead of launching the terminal dashboard")
	cmd.AddCommand(
		newDoctorCommand(&configPath, &host, &port, opts),
		newInitCommand(&configPath, opts),
		newAddProjectCommand(&configPath, opts),
		newEditProjectCommand(&configPath, opts, OperationPauseProject, "pause", "Pause a project", func(project *globalconfig.Project) error {
			project.Paused = true
			return nil
		}),
		newEditProjectCommand(&configPath, opts, OperationUnpauseProject, "unpause", "Unpause a project", func(project *globalconfig.Project) error {
			project.Paused = false
			return nil
		}),
		newConfigCommand(&configPath, opts),
		newPromoteCommand(&configPath, opts),
		newRemoveProjectCommand(&configPath, opts),
	)

	return cmd
}

func defaultOptions() options {
	return options{
		resolvePath: globalconfig.ResolvePath,
		read: func(path string) (globalconfig.Config, error) {
			return globalconfig.Read(path)
		},
		readOrDefault: func(path string) (globalconfig.Config, error) {
			return globalconfig.ReadOrDefault(path, globalconfig.WithProjectPathLiterals())
		},
		write: func(path string, cfg globalconfig.Config) error {
			return globalconfig.Write(path, cfg, globalconfig.WithProjectPathLiterals())
		},
		boot:      defaultBoot,
		signal:    noSignal,
		stdoutTTY: stdoutIsTTY,
	}
}

func noSignal(context.Context, Signal) error {
	return nil
}

func stdoutIsTTY() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func newInitCommand(configPath *string, opts options) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a default global config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := resolveConfigPath(*configPath, opts)
			if err != nil {
				return err
			}

			cfg, err := globalconfig.DefaultAt(path)
			if err != nil {
				return err
			}
			path = cfg.Path
			if err := checkInitTarget(path, force); err != nil {
				return err
			}
			if err := opts.write(path, cfg); err != nil {
				return err
			}
			return opts.signal(cmd.Context(), Signal{
				Operation: OperationInit,
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing global config")
	return cmd
}

func checkInitTarget(path string, force bool) error {
	_, err := os.Stat(path)
	if err == nil {
		if !force {
			return fmt.Errorf("%w: %s", ErrConfigExists, path)
		}
		if _, readErr := os.ReadFile(path); readErr != nil {
			return fmt.Errorf("read existing global config %s: %w", path, readErr)
		}
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("stat global config %s: %w", path, err)
}

func newAddProjectCommand(configPath *string, opts options) *cobra.Command {
	var cfg globalconfig.Project
	cmd := &cobra.Command{
		Use:   "add-project",
		Short: "Add a project to global config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := resolveConfigPath(*configPath, opts)
			if err != nil {
				return err
			}
			cfg.ID = strings.TrimSpace(cfg.ID)
			cfg.CredentialRef = strings.TrimSpace(cfg.CredentialRef)
			if err := validateProjectFlags(cfg); err != nil {
				return err
			}

			global, err := opts.readOrDefault(path)
			if err != nil {
				return err
			}
			if projectIndex(global.Projects, cfg.ID) >= 0 {
				return fmt.Errorf("%w: %s", ErrProjectExists, cfg.ID)
			}

			global.Projects = append(global.Projects, cfg)
			if err := opts.write(path, global); err != nil {
				return err
			}
			return opts.signal(cmd.Context(), Signal{
				Operation: OperationAddProject,
				ProjectID: cfg.ID,
				Project:   cfg,
			})
		},
	}
	cmd.Flags().StringVar(&cfg.ID, "id", "", "project id")
	cmd.Flags().StringVar(&cfg.Workflow, "workflow", "", "project workflow path")
	cmd.Flags().StringVar(&cfg.Workdir, "workdir", "", "project worktree root")
	cmd.Flags().IntVar(&cfg.Weight, "weight", 1, "project scheduling weight")
	cmd.Flags().IntVar(&cfg.Priority, "priority", 0, "project dispatch priority")
	cmd.Flags().BoolVar(&cfg.Paused, "paused", false, "add the project in a paused state")
	cmd.Flags().StringVar(&cfg.CredentialRef, "credential-ref", "", "project credential reference")
	return cmd
}

func validateProjectFlags(cfg globalconfig.Project) error {
	switch {
	case cfg.ID == "":
		return errors.New("project id is required")
	case strings.TrimSpace(cfg.Workflow) == "":
		return errors.New("project workflow is required")
	case strings.TrimSpace(cfg.Workdir) == "":
		return errors.New("project workdir is required")
	case cfg.Weight <= 0:
		return errors.New("project weight must be positive")
	default:
		return nil
	}
}

type projectEdit func(*globalconfig.Project) error

func newEditProjectCommand(configPath *string, opts options, operation Operation, use string, short string, edit projectEdit) *cobra.Command {
	return &cobra.Command{
		Use:   use + " PROJECT_ID",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return updateProject(cmd.Context(), *configPath, opts, operation, args[0], edit)
		},
	}
}

func newConfigCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect global config settings",
	}
	cmd.AddCommand(newConfigPathCommand(configPath, opts))
	return cmd
}

func newConfigPathCommand(configPath *string, opts options) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the resolved global config path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolution, err := resolveConfigPathResolution(*configPath, opts)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "path: %s\nrule: %s\n", resolution.Path, resolution.Rule)
			return err
		},
	}
}

func newPromoteCommand(configPath *string, opts options) *cobra.Command {
	var priority int
	cmd := &cobra.Command{
		Use:   "promote PROJECT_ID",
		Short: "Promote a project priority",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if priority <= 0 {
				return errors.New("priority must be positive")
			}
			return updateProject(cmd.Context(), *configPath, opts, OperationPromoteProject, args[0], func(project *globalconfig.Project) error {
				project.Priority = priority
				return nil
			})
		},
	}
	cmd.Flags().IntVar(&priority, "priority", 1, "project priority rank")
	return cmd
}

func updateProject(ctx context.Context, configPath string, opts options, operation Operation, id string, edit projectEdit) error {
	path, err := resolveConfigPath(configPath, opts)
	if err != nil {
		return err
	}
	cfg, err := opts.readOrDefault(path)
	if err != nil {
		return err
	}

	id = strings.TrimSpace(id)
	index := projectIndex(cfg.Projects, id)
	if index < 0 {
		return fmt.Errorf("%w: %s", ErrProjectNotFound, id)
	}
	if err := edit(&cfg.Projects[index]); err != nil {
		return err
	}
	if err := opts.write(path, cfg); err != nil {
		return err
	}

	return opts.signal(ctx, Signal{
		Operation: operation,
		ProjectID: cfg.Projects[index].ID,
		Project:   cfg.Projects[index],
	})
}

func newRemoveProjectCommand(configPath *string, opts options) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-project PROJECT_ID",
		Short: "Remove a project from global config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(*configPath, opts)
			if err != nil {
				return err
			}
			cfg, err := opts.readOrDefault(path)
			if err != nil {
				return err
			}

			id := strings.TrimSpace(args[0])
			index := projectIndex(cfg.Projects, id)
			if index < 0 {
				return fmt.Errorf("%w: %s", ErrProjectNotFound, id)
			}
			removed := cfg.Projects[index]
			cfg.Projects = append(cfg.Projects[:index], cfg.Projects[index+1:]...)
			if cfg.Projects == nil {
				cfg.Projects = []globalconfig.Project{}
			}
			if err := opts.write(path, cfg); err != nil {
				return err
			}

			return opts.signal(cmd.Context(), Signal{
				Operation: OperationRemoveProject,
				ProjectID: removed.ID,
				Project:   removed,
			})
		},
	}
}

func resolveConfigPath(path string, opts options) (string, error) {
	resolution, err := resolveConfigPathResolution(path, opts)
	if err != nil {
		return "", err
	}
	return resolution.Path, nil
}

func resolveConfigPathResolution(path string, opts options) (globalconfig.PathResolution, error) {
	if opts.resolvePath == nil {
		opts.resolvePath = globalconfig.ResolvePath
	}
	return opts.resolvePath(path)
}

func projectIndex(projects []globalconfig.Project, id string) int {
	id = strings.TrimSpace(id)
	for index, project := range projects {
		if strings.TrimSpace(project.ID) == id {
			return index
		}
	}
	return -1
}
