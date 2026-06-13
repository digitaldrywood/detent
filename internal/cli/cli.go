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

	"github.com/digitaldrywood/detent/internal/buildinfo"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
)

var (
	ErrConfigExists    = errors.New("global config already exists")
	ErrProjectExists   = errors.New("project already exists")
	ErrProjectNotFound = errors.New("project not found")
)

const (
	addProjectExampleCommand = "detent add-project --id api --workflow ./WORKFLOW.md --workdir ~/code/api"
	configPathCommand        = "detent config path"
	forceInitCommand         = "detent init --force"
	ghAuthLoginCommand       = `gh auth login --scopes "repo,read:org,project"`
	portExampleCommand       = "detent --port 0"
	promoteExampleCommand    = "detent promote api --priority 10"
)

type HintedError struct {
	Err      error
	Message  string
	Hint     string
	Commands []string
}

func (e *HintedError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return ""
}

func (e *HintedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func HintFor(err error) (string, []string, bool) {
	var hinted *HintedError
	if !errors.As(err, &hinted) {
		return "", nil, false
	}
	return hinted.Hint, append([]string(nil), hinted.Commands...), true
}

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

type initResult struct {
	Status string `json:"status"`
	Path   string `json:"path"`
	Rule   string `json:"rule"`
}

type configPathResult struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

type projectResult struct {
	ID            string `json:"id"`
	Workflow      string `json:"workflow"`
	Workdir       string `json:"workdir"`
	Weight        int    `json:"weight"`
	Priority      int    `json:"priority"`
	Paused        bool   `json:"paused"`
	CredentialRef string `json:"credential_ref,omitempty"`
}

type projectPausedResult struct {
	Status  string `json:"status"`
	Project string `json:"project"`
	Paused  bool   `json:"paused"`
}

type projectPriorityResult struct {
	Status   string `json:"status"`
	Project  string `json:"project"`
	Priority int    `json:"priority"`
}

type projectRemovedResult struct {
	Status  string `json:"status"`
	Project string `json:"project"`
	Removed bool   `json:"removed"`
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
	Runtime        RuntimeSettings
	WorkflowPath   string
	Host           string
	Port           *int
	Version        string
	Build          buildinfo.Info
	Headless       bool
	StdoutTTY      bool
	Output         io.Writer
	Shutdown       *ShutdownController
}

type BootFunc func(context.Context, BootConfig) error

type SignalFunc func(context.Context, Signal) error

type LoggerFunc func(RuntimeSettings, io.Writer, io.Writer, bool)

type ProjectManager interface {
	Add(context.Context, globalconfig.Project) error
	Remove(context.Context, project.ID) error
	Pause(context.Context, project.ID) error
	Unpause(context.Context, project.ID) error
}

type Option func(*options)

type options struct {
	resolvePath   func(string) (globalconfig.PathResolution, error)
	read          func(string) (globalconfig.Config, error)
	readOrDefault func(string) (globalconfig.Config, error)
	write         func(string, globalconfig.Config) error
	boot          BootFunc
	signal        SignalFunc
	lookupEnv     func(string) string
	ghAuthToken   func(context.Context) (string, error)
	configureLog  LoggerFunc
	version       string
	build         buildinfo.Info
	stdoutTTY     func() bool
	shutdown      *ShutdownController
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

func WithBuild(build buildinfo.Info) Option {
	return func(opts *options) {
		opts.build = build
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

func WithShutdownController(controller *ShutdownController) Option {
	return func(opts *options) {
		opts.shutdown = controller
	}
}

func WithLoggerFunc(configure LoggerFunc) Option {
	return func(opts *options) {
		opts.configureLog = configure
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
			return manager.Remove(ctx, project.ID(signal.ProjectID))
		case OperationPauseProject:
			return manager.Pause(ctx, project.ID(signal.ProjectID))
		case OperationUnpauseProject:
			return manager.Unpause(ctx, project.ID(signal.ProjectID))
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
	var env string
	var logLevel string
	var host string
	var port int
	var headless bool
	var outputFormat string
	cmd := &cobra.Command{
		Use:                        "detent",
		Short:                      "Detent agent orchestrator",
		Long:                       "Detent is an agent orchestrator for tracker-backed work queues.",
		Args:                       suggestedNoArgs,
		SilenceUsage:               true,
		SilenceErrors:              true,
		SuggestionsMinimumDistance: 2,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := OutputForCommand(cmd); err != nil {
				return err
			}
			flags := runtimeFlags{
				Env:      runtimeStringFlag{Value: env, Set: flagChanged(cmd, "env")},
				LogLevel: runtimeStringFlag{Value: logLevel, Set: flagChanged(cmd, "log-level")},
				Port:     runtimeIntFlag{Value: port, Set: flagChanged(cmd, "port")},
			}
			boot, err := resolveBootConfig(cmd.Context(), configPath, host, flags, opts)
			if err != nil {
				return err
			}
			stdoutTTY := opts.stdoutTTY()
			if opts.configureLog != nil {
				opts.configureLog(boot.Runtime, cmd.OutOrStdout(), cmd.ErrOrStderr(), stdoutTTY)
			}
			boot.Headless = headless
			boot.StdoutTTY = stdoutTTY
			boot.Output = cmd.OutOrStdout()
			boot.Shutdown = opts.shutdown
			slog.Info("resolved global config", "path", boot.Global.Path, "rule", boot.ConfigPathRule)
			for _, warning := range boot.Runtime.Warnings {
				slog.Warn(warning.Detail, "check", warning.Name, "hint", warning.Hint)
			}
			return opts.boot(cmd.Context(), boot)
		},
	}
	cmd.SetContext(withCommandOutputOptions(ctx, commandOutputOptions{
		lookupEnv: opts.lookupEnv,
		stdoutTTY: opts.stdoutTTY,
	}))
	cmd.PersistentFlags().StringVar(&configPath, "config", "", "path to global.yaml")
	cmd.PersistentFlags().StringVar(&env, "env", "", "runtime environment")
	cmd.PersistentFlags().StringVar(&logLevel, "log-level", "", "log level")
	cmd.PersistentFlags().StringVar(&host, "host", "", "web server host")
	cmd.PersistentFlags().IntVar(&port, "port", -1, "web server port, or 0 for an ephemeral port")
	cmd.PersistentFlags().BoolVar(&headless, "headless", false, "stream logs instead of launching the terminal dashboard")
	cmd.PersistentFlags().Var(newOutputFormatValue(&outputFormat), outputFormatFlagName, "output format: pretty or json (default: pretty on TTY, json when piped; DETENT_FORMAT overrides)")
	cmd.SetFlagErrorFunc(flagSuggestionError)

	addProjectCommand := newAddProjectCommand(&configPath, opts)
	addProjectCommand.SuggestFor = []string{"add", "new"}
	pauseCommand := newEditProjectCommand(&configPath, opts, OperationPauseProject, "pause", "Pause a project", func(project *globalconfig.Project) error {
		project.Paused = true
		return nil
	})
	pauseCommand.SuggestFor = []string{"stop"}
	unpauseCommand := newEditProjectCommand(&configPath, opts, OperationUnpauseProject, "unpause", "Unpause a project", func(project *globalconfig.Project) error {
		project.Paused = false
		return nil
	})
	unpauseCommand.SuggestFor = []string{"resume", "start"}
	promoteCommand := newPromoteCommand(&configPath, opts)
	promoteCommand.SuggestFor = []string{"prioritize"}
	removeProjectCommand := newRemoveProjectCommand(&configPath, opts)
	removeProjectCommand.SuggestFor = []string{"rm", "delete", "remove"}
	cmd.AddCommand(
		newDoctorCommand(&configPath, &env, &logLevel, &host, &port, opts),
		newInitCommand(&configPath, opts),
		addProjectCommand,
		pauseCommand,
		unpauseCommand,
		newConfigCommand(&configPath, opts),
		promoteCommand,
		removeProjectCommand,
	)
	wrapHintedErrors(cmd)

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
		boot:        defaultBoot,
		signal:      noSignal,
		lookupEnv:   os.Getenv,
		ghAuthToken: defaultGHAuthToken,
		stdoutTTY:   stdoutIsTTY,
	}
}

func flagChanged(cmd *cobra.Command, name string) bool {
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		flag = cmd.InheritedFlags().Lookup(name)
	}
	return flag != nil && flag.Changed
}

func noSignal(context.Context, Signal) error {
	return nil
}

func stdoutIsTTY() bool {
	return WriterIsTTY(os.Stdout)
}

func hintedError(cause error, message string, hint string, commands ...string) error {
	return &HintedError{
		Err:      cause,
		Message:  message,
		Hint:     strings.TrimSpace(hint),
		Commands: append([]string(nil), commands...),
	}
}

func exampleHint(command string) string {
	return "e.g. " + command
}

func wrapHintedErrors(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	if cmd.RunE != nil {
		runE := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			err := runE(cmd, args)
			if err != nil {
				if writeErr := writeErrorHint(cmd.ErrOrStderr(), err); writeErr != nil {
					return errors.Join(err, writeErr)
				}
			}
			return err
		}
	}
	for _, child := range cmd.Commands() {
		wrapHintedErrors(child)
	}
}

func writeErrorHint(out io.Writer, err error) error {
	hint, _, ok := HintFor(err)
	if !ok || strings.TrimSpace(hint) == "" {
		return nil
	}
	_, writeErr := fmt.Fprintf(out, "Hint: %s\n", hint)
	return writeErr
}

func newInitCommand(configPath *string, opts options) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a default global config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			resolution, err := resolveConfigPathResolution(*configPath, opts)
			if err != nil {
				return err
			}
			path := resolution.Path

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
			if err := opts.signal(cmd.Context(), Signal{
				Operation: OperationInit,
			}); err != nil {
				return err
			}
			return out.Write(nil, initResult{
				Status: "ok",
				Path:   path,
				Rule:   string(resolution.Rule),
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
			return hintedError(
				ErrConfigExists,
				fmt.Sprintf("%s: %s", ErrConfigExists, path),
				"run detent init --force to overwrite it, or edit the file reported by detent config path",
				forceInitCommand,
				configPathCommand,
			)
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
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
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
				return projectExistsError(cfg.ID)
			}

			global.Projects = append(global.Projects, cfg)
			if err := opts.write(path, global); err != nil {
				return err
			}
			if err := opts.signal(cmd.Context(), Signal{
				Operation: OperationAddProject,
				ProjectID: cfg.ID,
				Project:   cfg,
			}); err != nil {
				return err
			}
			return out.Write(nil, newProjectResult(cfg))
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
		return hintedError(nil, "--id is required", exampleHint(addProjectExampleCommand), addProjectExampleCommand)
	case strings.TrimSpace(cfg.Workflow) == "":
		return hintedError(nil, "--workflow is required", exampleHint(addProjectExampleCommand), addProjectExampleCommand)
	case strings.TrimSpace(cfg.Workdir) == "":
		return hintedError(nil, "--workdir is required", exampleHint(addProjectExampleCommand), addProjectExampleCommand)
	case cfg.Weight <= 0:
		command := addProjectExampleCommand + " --weight 1"
		return hintedError(nil, "--weight must be positive", exampleHint(command), command)
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
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			updated, err := updateProject(cmd.Context(), *configPath, opts, operation, args[0], edit)
			if err != nil {
				return err
			}
			return out.Write(nil, projectEditResult(operation, updated))
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
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			resolution, err := resolveConfigPathResolution(*configPath, opts)
			if err != nil {
				return err
			}
			return out.Write(func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "path: %s\nrule: %s\n", resolution.Path, resolution.Rule)
				return err
			}, configPathResult{
				Path: resolution.Path,
				Rule: string(resolution.Rule),
			})
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
				return hintedError(nil, "--priority must be positive", exampleHint(promoteExampleCommand), promoteExampleCommand)
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			updated, err := updateProject(cmd.Context(), *configPath, opts, OperationPromoteProject, args[0], func(project *globalconfig.Project) error {
				project.Priority = priority
				return nil
			})
			if err != nil {
				return err
			}
			return out.Write(nil, projectPriorityResult{
				Status:   "ok",
				Project:  updated.ID,
				Priority: updated.Priority,
			})
		},
	}
	cmd.Flags().IntVar(&priority, "priority", 1, "project priority rank")
	return cmd
}

func updateProject(ctx context.Context, configPath string, opts options, operation Operation, id string, edit projectEdit) (globalconfig.Project, error) {
	path, err := resolveConfigPath(configPath, opts)
	if err != nil {
		return globalconfig.Project{}, err
	}
	cfg, err := opts.readOrDefault(path)
	if err != nil {
		return globalconfig.Project{}, err
	}

	id = strings.TrimSpace(id)
	index := projectIndex(cfg.Projects, id)
	if index < 0 {
		return globalconfig.Project{}, projectNotFoundError(id, cfg.Projects)
	}
	if err := edit(&cfg.Projects[index]); err != nil {
		return globalconfig.Project{}, err
	}
	if err := opts.write(path, cfg); err != nil {
		return globalconfig.Project{}, err
	}

	if err := opts.signal(ctx, Signal{
		Operation: operation,
		ProjectID: cfg.Projects[index].ID,
		Project:   cfg.Projects[index],
	}); err != nil {
		return globalconfig.Project{}, err
	}
	return cfg.Projects[index], nil
}

func newRemoveProjectCommand(configPath *string, opts options) *cobra.Command {
	return &cobra.Command{
		Use:   "remove-project PROJECT_ID",
		Short: "Remove a project from global config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
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
				return projectNotFoundError(id, cfg.Projects)
			}
			removed := cfg.Projects[index]
			cfg.Projects = append(cfg.Projects[:index], cfg.Projects[index+1:]...)
			if cfg.Projects == nil {
				cfg.Projects = []globalconfig.Project{}
			}
			if err := opts.write(path, cfg); err != nil {
				return err
			}

			if err := opts.signal(cmd.Context(), Signal{
				Operation: OperationRemoveProject,
				ProjectID: removed.ID,
				Project:   removed,
			}); err != nil {
				return err
			}
			return out.Write(nil, projectRemovedResult{
				Status:  "ok",
				Project: removed.ID,
				Removed: true,
			})
		},
	}
}

func newProjectResult(project globalconfig.Project) projectResult {
	return projectResult{
		ID:            project.ID,
		Workflow:      project.Workflow,
		Workdir:       project.Workdir,
		Weight:        project.Weight,
		Priority:      project.Priority,
		Paused:        project.Paused,
		CredentialRef: project.CredentialRef,
	}
}

func projectEditResult(operation Operation, project globalconfig.Project) any {
	switch operation {
	case OperationPauseProject, OperationUnpauseProject:
		return projectPausedResult{
			Status:  "ok",
			Project: project.ID,
			Paused:  project.Paused,
		}
	default:
		return newProjectResult(project)
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

func projectExistsError(id string) error {
	id = strings.TrimSpace(id)
	return hintedError(
		ErrProjectExists,
		fmt.Sprintf("project %q already exists", id),
		fmt.Sprintf("project id %q is already taken; run detent config path to inspect current projects before choosing a new --id", id),
		configPathCommand,
	)
}

func projectNotFoundError(id string, projects []globalconfig.Project) error {
	id = strings.TrimSpace(id)
	ids := projectHintIDs(projects)
	if len(ids) == 0 {
		return hintedError(
			ErrProjectNotFound,
			fmt.Sprintf("project %q not found", id),
			"no projects are configured; run detent add-project to add one",
			addProjectExampleCommand,
		)
	}

	hint := "available: " + strings.Join(ids, ", ")
	if closest := closestProjectID(id, ids); closest != "" {
		hint += fmt.Sprintf("\ndid you mean %q? see `%s`, then retry", closest, configPathCommand)
	} else {
		hint += fmt.Sprintf("\nsee `%s`, then retry", configPathCommand)
	}
	return hintedError(ErrProjectNotFound, fmt.Sprintf("project %q not found", id), hint, configPathCommand)
}

func projectHintIDs(projects []globalconfig.Project) []string {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		id := strings.TrimSpace(project.ID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func closestProjectID(target string, ids []string) string {
	target = strings.TrimSpace(target)
	bestID := ""
	bestDistance := 0
	for _, id := range ids {
		distance := levenshteinDistance(target, id)
		if bestID == "" || distance < bestDistance {
			bestID = id
			bestDistance = distance
		}
	}
	return bestID
}

func levenshteinDistance(a string, b string) int {
	ar := []rune(strings.ToLower(a))
	br := []rune(strings.ToLower(b))
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}

	previous := make([]int, len(br)+1)
	for j := range previous {
		previous[j] = j
	}
	for i, ra := range ar {
		current := make([]int, len(br)+1)
		current[0] = i + 1
		for j, rb := range br {
			cost := 0
			if ra != rb {
				cost = 1
			}
			current[j+1] = minInt(
				current[j]+1,
				previous[j+1]+1,
				previous[j]+cost,
			)
		}
		previous = current
	}
	return previous[len(br)]
}

func minInt(values ...int) int {
	min := values[0]
	for _, value := range values[1:] {
		if value < min {
			min = value
		}
	}
	return min
}
