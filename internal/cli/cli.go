package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/digitaldrywood/detent/internal/buildinfo"
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

const (
	outputFormatPretty = "pretty"
	outputFormatJSON   = "json"
)

const rootLongHelp = `Detent is an agent orchestrator for tracker-backed work queues.

Exit Codes:
  0  success
  1  general or unexpected error
  2  authentication or GitHub token problem
  3  input validation error
  4  not found or config conflict

Output Format:
  --format pretty writes human-readable output.
  --format json writes machine-readable JSON where supported; with help it writes the command catalog.
  DETENT_FORMAT can set the default output format for commands that honor shared output resolution.
  A non-TTY stdout defaults to JSON for shared-output commands; use --format pretty to force text.`

const examplesFirstUsageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

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
	outputFormat := outputFormatPretty
	cmd := &cobra.Command{
		Use:          "detent",
		Short:        "Detent agent orchestrator",
		Long:         rootLongHelp,
		Example:      "  detent --config ~/.config/detent/global.yaml --headless\n  detent --format json help",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return validateOutputFormat(outputFormat)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
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
	cmd.SetContext(ctx)
	cmd.SetUsageTemplate(examplesFirstUsageTemplate)
	cmd.SetHelpFunc(newExamplesFirstHelpFunc(&outputFormat))
	cmd.PersistentFlags().StringVar(&configPath, "config", "", "path to global.yaml")
	cmd.PersistentFlags().StringVar(&env, "env", "", "runtime environment")
	cmd.PersistentFlags().StringVar(&outputFormat, "format", outputFormatPretty, "output format (pretty|json)")
	cmd.PersistentFlags().StringVar(&logLevel, "log-level", "", "log level")
	cmd.PersistentFlags().StringVar(&host, "host", "", "web server host")
	cmd.PersistentFlags().IntVar(&port, "port", -1, "web server port, or 0 for an ephemeral port")
	cmd.PersistentFlags().BoolVar(&headless, "headless", false, "stream logs instead of launching the terminal dashboard")
	cmd.AddCommand(
		newDoctorCommand(&configPath, &env, &logLevel, &host, &port, opts),
		newInitCommand(&configPath, opts),
		newAddProjectCommand(&configPath, opts),
		newEditProjectCommand(&configPath, opts, OperationPauseProject, "pause", "Pause a project", "  detent pause api", func(project *globalconfig.Project) error {
			project.Paused = true
			return nil
		}),
		newEditProjectCommand(&configPath, opts, OperationUnpauseProject, "unpause", "Unpause a project", "  detent unpause api", func(project *globalconfig.Project) error {
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
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func newInitCommand(configPath *string, opts options) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "init",
		Short:   "Create a default global config",
		Example: "  detent init --config ~/.config/detent/global.yaml",
		Args:    cobra.NoArgs,
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
		Use:     "add-project",
		Short:   "Add a project to global config",
		Example: "  detent add-project --id api --workflow ./WORKFLOW.md --workdir ~/code/api --weight 2",
		Args:    cobra.NoArgs,
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

func newEditProjectCommand(configPath *string, opts options, operation Operation, use string, short string, example string, edit projectEdit) *cobra.Command {
	return &cobra.Command{
		Use:     use + " PROJECT_ID",
		Short:   short,
		Example: example,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return updateProject(cmd.Context(), *configPath, opts, operation, args[0], edit)
		},
	}
}

func newConfigCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "config",
		Short:   "Inspect global config settings",
		Example: "  detent config path",
	}
	cmd.AddCommand(newConfigPathCommand(configPath, opts))
	return cmd
}

func newConfigPathCommand(configPath *string, opts options) *cobra.Command {
	return &cobra.Command{
		Use:     "path",
		Short:   "Print the resolved global config path",
		Example: "  detent config path --config ~/.config/detent/global.yaml",
		Args:    cobra.NoArgs,
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
		Use:     "promote PROJECT_ID",
		Short:   "Promote a project priority",
		Example: "  detent promote api --priority 1",
		Args:    cobra.ExactArgs(1),
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
		Use:     "remove-project PROJECT_ID",
		Short:   "Remove a project from global config",
		Example: "  detent remove-project api",
		Args:    cobra.ExactArgs(1),
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

func validateOutputFormat(format string) error {
	switch normalizedOutputFormat(format) {
	case outputFormatPretty, outputFormatJSON:
		return nil
	default:
		return fmt.Errorf("unsupported output format %q: use pretty or json", format)
	}
}

func normalizedOutputFormat(format string) string {
	return strings.ToLower(strings.TrimSpace(format))
}

func newExamplesFirstHelpFunc(format *string) func(*cobra.Command, []string) {
	return func(cmd *cobra.Command, _ []string) {
		switch normalizedOutputFormat(derefString(format)) {
		case outputFormatJSON:
			if err := writeCommandCatalogJSON(cmd.OutOrStdout(), cmd.Root()); err != nil {
				cmd.PrintErrln(err)
			}
		case outputFormatPretty, "":
			if err := writeExamplesFirstHelp(cmd.OutOrStdout(), cmd); err != nil {
				cmd.PrintErrln(err)
			}
		default:
			cmd.PrintErrln(validateOutputFormat(derefString(format)))
		}
	}
}

func writeExamplesFirstHelp(out io.Writer, cmd *cobra.Command) error {
	if strings.TrimSpace(cmd.Example) != "" {
		if _, err := fmt.Fprintf(out, "Examples:\n%s\n\n", strings.TrimRight(cmd.Example, "\n")); err != nil {
			return err
		}
	}

	description := strings.TrimSpace(cmd.Long)
	if description == "" {
		description = strings.TrimSpace(cmd.Short)
	}
	if description != "" {
		if _, err := fmt.Fprintf(out, "%s\n\n", description); err != nil {
			return err
		}
	}

	if cmd.Runnable() || cmd.HasSubCommands() {
		_, err := fmt.Fprint(out, cmd.UsageString())
		return err
	}
	return nil
}

type commandCatalog struct {
	Commands []commandCatalogEntry `json:"commands"`
}

type commandCatalogEntry struct {
	Name    string               `json:"name"`
	Short   string               `json:"short"`
	Args    string               `json:"args"`
	Example string               `json:"example"`
	Flags   []commandCatalogFlag `json:"flags"`
}

type commandCatalogFlag struct {
	Name      string `json:"name"`
	Usage     string `json:"usage"`
	Type      string `json:"type"`
	Default   string `json:"default"`
	Inherited bool   `json:"inherited"`
}

func writeCommandCatalogJSON(out io.Writer, root *cobra.Command) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(buildCommandCatalog(root))
}

func buildCommandCatalog(root *cobra.Command) commandCatalog {
	var commands []commandCatalogEntry
	walkCatalogCommands(root, func(cmd *cobra.Command) {
		if isGeneratedCatalogCommand(cmd) || cmd.Hidden {
			return
		}
		cmd.InitDefaultHelpFlag()
		cmd.InitDefaultVersionFlag()
		commands = append(commands, commandCatalogEntry{
			Name:    cmd.CommandPath(),
			Short:   cmd.Short,
			Args:    commandArgs(cmd),
			Example: catalogExample(cmd.Example),
			Flags:   catalogFlags(cmd),
		})
	})
	sort.Slice(commands, func(i, j int) bool {
		return commands[i].Name < commands[j].Name
	})
	return commandCatalog{Commands: commands}
}

func walkCatalogCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	if isGeneratedCatalogCommand(cmd) || cmd.Hidden {
		return
	}
	visit(cmd)
	for _, child := range cmd.Commands() {
		walkCatalogCommands(child, visit)
	}
}

func isGeneratedCatalogCommand(cmd *cobra.Command) bool {
	name := cmd.Name()
	return name == "help" || name == "completion" || strings.HasPrefix(name, cobra.ShellCompRequestCmd)
}

func commandArgs(cmd *cobra.Command) string {
	fields := strings.Fields(cmd.Use)
	if len(fields) <= 1 {
		return ""
	}
	return strings.Join(fields[1:], " ")
}

func catalogExample(example string) string {
	lines := strings.Split(strings.TrimSpace(example), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.Join(lines, "\n")
}

func catalogFlags(cmd *cobra.Command) []commandCatalogFlag {
	flags := collectCatalogFlags(cmd.LocalFlags(), false)
	flags = append(flags, collectCatalogFlags(cmd.InheritedFlags(), true)...)
	sort.Slice(flags, func(i, j int) bool {
		if flags[i].Name == flags[j].Name {
			return !flags[i].Inherited && flags[j].Inherited
		}
		return flags[i].Name < flags[j].Name
	})
	return flags
}

func collectCatalogFlags(flagSet *pflag.FlagSet, inherited bool) []commandCatalogFlag {
	var flags []commandCatalogFlag
	flagSet.VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden {
			return
		}
		flags = append(flags, commandCatalogFlag{
			Name:      flag.Name,
			Usage:     flag.Usage,
			Type:      flag.Value.Type(),
			Default:   flag.DefValue,
			Inherited: inherited,
		})
	})
	return flags
}
