package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

var ErrDoctorFailed = errors.New("doctor found failed checks")

const doctorCommandTimeout = 5 * time.Second

type doctorStatus string

const (
	doctorOK   doctorStatus = "OK"
	doctorWarn doctorStatus = "WARN"
	doctorFail doctorStatus = "FAIL"
)

var requiredGitHubScopes = []string{"repo", "read:org", "project"}

type doctorCheck struct {
	Name   string
	Status doctorStatus
	Detail string
	Hint   string
}

type doctorReport struct {
	Checks []doctorCheck
}

type doctorConfig struct {
	ConfigPath string
	Host       string
	Flags      runtimeFlags
}

type doctorStore interface {
	Close() error
}

type doctorDeps struct {
	loadWorkflow func(string) (workflowconfig.Workflow, error)
	lookupEnv    func(string) string
	lookPath     func(string) (string, error)
	runCommand   func(context.Context, string, ...string) error
	githubScopes func(context.Context, string) ([]string, error)
	ghAuthToken  func(context.Context) (string, error)
	listen       func(string, string) (net.Listener, error)
	openSQLite   func(context.Context, string) (doctorStore, error)
	gitWorkTree  func(context.Context, string) error
}

func newDoctorCommand(configPath *string, env *string, logLevel *string, host *string, port *int, opts options) *cobra.Command {
	return newDoctorCommandWithDeps(configPath, env, logLevel, host, port, opts, doctorDeps{})
}

func newDoctorCommandWithDeps(configPath *string, env *string, logLevel *string, host *string, port *int, opts options, deps doctorDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight health checks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := runDoctor(cmd.Context(), doctorConfig{
				ConfigPath: derefString(configPath),
				Host:       derefString(host),
				Flags: runtimeFlags{
					Env:      runtimeStringFlag{Value: derefString(env), Set: flagChanged(cmd, "env")},
					LogLevel: runtimeStringFlag{Value: derefString(logLevel), Set: flagChanged(cmd, "log-level")},
					Port:     runtimeIntFlag{Value: derefInt(port, -1), Set: flagChanged(cmd, "port")},
				},
			}, opts, deps)
			if err := writeDoctorReport(cmd.OutOrStdout(), report); err != nil {
				return err
			}
			if report.HasFailures() {
				return ErrDoctorFailed
			}
			return nil
		},
	}
}

func runDoctor(ctx context.Context, cfg doctorConfig, opts options, deps doctorDeps) doctorReport {
	if ctx == nil {
		ctx = context.Background()
	}
	opts = doctorOptions(opts)
	deps = deps.withDefaults()

	var report doctorReport
	resolution, global, configCheck := checkDoctorConfig(cfg.ConfigPath, opts)
	report.Add(configCheck)

	workflowPath := ""
	if global != nil {
		workflowPath = firstGlobalWorkflowPath(*global)
	}
	runtime, runtimeErr := resolveRuntimeSettings(ctx, runtimeInput{
		Config:     global,
		ConfigPath: resolution,
		Workflow:   workflowPath,
		Flags:      cfg.Flags,
	}, runtimeDeps{
		lookupEnv:    deps.lookupEnv,
		ghAuthToken:  deps.ghAuthToken,
		loadWorkflow: deps.loadWorkflow,
	})
	if runtimeErr != nil {
		report.Add(doctorCheck{
			Name:   "Runtime settings",
			Status: doctorFail,
			Detail: runtimeErr.Error(),
			Hint:   "Fix runtime flags, environment variables, or global.yaml.",
		})
	} else {
		report.Add(checkDoctorRuntimeSettings(runtime))
	}

	boot := BootConfig{
		Host: strings.TrimSpace(cfg.Host),
		Port: bootPort(cfg.Flags.Port.Value),
	}
	if runtimeErr == nil {
		port := runtime.Port.Value
		boot.Port = &port
	}
	if global != nil {
		boot.Global = *global
		boot.Host = bootHost(cfg.Host, firstGlobalWorkflowPath(*global))
		report.Add(checkDoctorInstanceIdentity(*global))
		report.Checks = append(report.Checks, checkDoctorProjects(ctx, *global, deps, runtime.GitHubToken.Value)...)
	} else {
		report.Add(doctorCheck{
			Name:   "Project workflows",
			Status: doctorWarn,
			Detail: "skipped because global config could not be loaded",
			Hint:   "Fix the global config, then rerun detent doctor.",
		})
	}

	report.Add(checkDoctorSQLite(ctx, resolution, deps))
	report.Add(checkDoctorCodex(ctx, deps))
	report.Add(checkDoctorGitHub(ctx, global, runtime.GitHubToken, deps))
	report.Add(checkDoctorServerPort(boot, deps))
	report.Add(checkDoctorGit(ctx, deps))

	return report
}

func (r *doctorReport) Add(check doctorCheck) {
	r.Checks = append(r.Checks, check)
}

func (r doctorReport) HasFailures() bool {
	for _, check := range r.Checks {
		if check.Status == doctorFail {
			return true
		}
	}
	return false
}

func (r doctorReport) counts() map[doctorStatus]int {
	counts := map[doctorStatus]int{
		doctorOK:   0,
		doctorWarn: 0,
		doctorFail: 0,
	}
	for _, check := range r.Checks {
		counts[check.Status]++
	}
	return counts
}

func writeDoctorReport(out io.Writer, report doctorReport) error {
	if out == nil {
		out = io.Discard
	}

	if _, err := fmt.Fprintln(out, "Detent Doctor"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "%-5s  %-28s  %s\n", "STATUS", "CHECK", "DETAIL"); err != nil {
		return err
	}
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(out, "%-5s  %-28s  %s\n", check.Status, check.Name, check.Detail); err != nil {
			return err
		}
		if strings.TrimSpace(check.Hint) != "" {
			if _, err := fmt.Fprintf(out, "%-5s  %-28s  Hint: %s\n", "", "", check.Hint); err != nil {
				return err
			}
		}
	}

	counts := report.counts()
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Summary: %d OK, %d WARN, %d FAIL\n", counts[doctorOK], counts[doctorWarn], counts[doctorFail]); err != nil {
		return err
	}
	result := "PASS"
	if counts[doctorFail] > 0 {
		result = "FAIL"
	}
	_, err := fmt.Fprintf(out, "Result: %s\n", result)
	return err
}

func checkDoctorConfig(configPath string, opts options) (globalconfig.PathResolution, *globalconfig.Config, doctorCheck) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return globalconfig.PathResolution{}, nil, doctorCheck{
			Name:   "Config resolution",
			Status: doctorFail,
			Detail: err.Error(),
			Hint:   "Pass --config or set CONFIG to a readable global.yaml.",
		}
	}

	cfg, err := opts.read(resolution.Path)
	if err != nil {
		return resolution, nil, doctorCheck{
			Name:   "Config resolution",
			Status: doctorFail,
			Detail: fmt.Sprintf("%s via %s; %v", resolution.Path, resolution.Rule, err),
			Hint:   "Run detent init or fix the global config file.",
		}
	}

	return resolution, &cfg, doctorCheck{
		Name:   "Config resolution",
		Status: doctorOK,
		Detail: fmt.Sprintf("%s via %s; %d project(s)", cfg.Path, resolution.Rule, len(cfg.Projects)),
	}
}

func checkDoctorRuntimeSettings(settings RuntimeSettings) doctorCheck {
	check := doctorCheck{
		Name:   "Runtime settings",
		Status: doctorOK,
		Detail: runtimeSettingsDetail(settings),
	}
	if len(settings.Warnings) == 0 {
		return check
	}

	check.Status = doctorWarn
	warning := settings.Warnings[0]
	check.Detail = check.Detail + "; " + warning.Detail
	check.Hint = warning.Hint
	return check
}

func checkDoctorProjects(ctx context.Context, cfg globalconfig.Config, deps doctorDeps, githubToken string) []doctorCheck {
	if len(cfg.Projects) == 0 {
		return []doctorCheck{
			{
				Name:   "Project workflows",
				Status: doctorWarn,
				Detail: "no projects configured",
				Hint:   "Run detent add-project to add a project.",
			},
		}
	}

	checks := make([]doctorCheck, 0, len(cfg.Projects)*2)
	for _, project := range cfg.Projects {
		id := strings.TrimSpace(project.ID)
		if id == "" {
			id = "project"
		}
		workflow, err := deps.loadWorkflow(project.Workflow)
		if err != nil {
			checks = append(checks, doctorCheck{
				Name:   "Project " + id + " workflow",
				Status: doctorFail,
				Detail: fmt.Sprintf("%s: %v", project.Workflow, err),
				Hint:   "Fix the WORKFLOW.md path or YAML frontmatter.",
			})
			checks = append(checks, doctorCheck{
				Name:   "Project " + id + " source repo",
				Status: doctorWarn,
				Detail: "skipped because WORKFLOW.md could not be loaded",
				Hint:   "Fix the workflow file, then rerun detent doctor.",
			})
			continue
		}
		workflow.Config = doctorWorkflowConfigWithRuntimeGitHubToken(workflow.Config, githubToken)
		if err := workflow.Config.Validate(); err != nil {
			checks = append(checks, doctorCheck{
				Name:   "Project " + id + " workflow",
				Status: doctorFail,
				Detail: fmt.Sprintf("%s: %v", project.Workflow, err),
				Hint:   "Fix invalid WORKFLOW.md frontmatter.",
			})
			checks = append(checks, doctorCheck{
				Name:   "Project " + id + " source repo",
				Status: doctorWarn,
				Detail: "skipped because WORKFLOW.md is invalid",
				Hint:   "Fix the workflow file, then rerun detent doctor.",
			})
			continue
		}

		checks = append(checks, doctorCheck{
			Name:   "Project " + id + " workflow",
			Status: doctorOK,
			Detail: doctorWorkflowDetail(project.Workflow, project, workflow.Config),
		})

		sourceRoot := projectSourceRoot(project, workflow.Config)
		if sourceRoot == "" {
			checks = append(checks, doctorCheck{
				Name:   "Project " + id + " source repo",
				Status: doctorFail,
				Detail: "source root is not configured",
				Hint:   "Set workspace.source_root, project workdir, or workspace.root to an existing git checkout.",
			})
			continue
		}
		expandedSourceRoot, err := expandDoctorWorkspacePath(sourceRoot)
		if err != nil {
			checks = append(checks, doctorCheck{
				Name:   "Project " + id + " source repo",
				Status: doctorFail,
				Detail: fmt.Sprintf("%s: %v", sourceRoot, err),
				Hint:   "Set workspace.source_root or project workdir to an existing git checkout.",
			})
			continue
		}
		if err := deps.gitWorkTree(ctx, expandedSourceRoot); err != nil {
			checks = append(checks, doctorCheck{
				Name:   "Project " + id + " source repo",
				Status: doctorFail,
				Detail: fmt.Sprintf("%s: %v", expandedSourceRoot, err),
				Hint:   "Set workspace.source_root or project workdir to an existing git checkout.",
			})
			continue
		}
		checks = append(checks, doctorCheck{
			Name:   "Project " + id + " source repo",
			Status: doctorOK,
			Detail: expandedSourceRoot + " is a git worktree",
		})
	}

	return checks
}

func doctorWorkflowConfigWithRuntimeGitHubToken(cfg workflowconfig.Config, token string) workflowconfig.Config {
	token = strings.TrimSpace(token)
	if token != "" && cfg.Tracker.Kind == workflowconfig.TrackerGitHub {
		cfg.Tracker.APIKey = token
	}
	return cfg
}

func checkDoctorInstanceIdentity(cfg globalconfig.Config) doctorCheck {
	return doctorCheck{
		Name:   "Instance identity",
		Status: doctorOK,
		Detail: doctorIdentityDetail(cfg.Global.Identity),
	}
}

func doctorWorkflowDetail(path string, project globalconfig.Project, cfg workflowconfig.Config) string {
	details := []string{path + " is valid"}
	if cfg.Identity.Configured() {
		details = append(details, "identity "+doctorIdentityDetail(cfg.Identity))
	}
	details = append(details, doctorAuthorizationDetail(project, cfg))
	return strings.Join(details, "; ")
}

func doctorIdentityDetail(identity workflowconfig.Identity) string {
	identity.Normalize()
	if !identity.Configured() {
		return "not configured; ownership defaults to assignee"
	}

	details := []string{identity.Name}
	if identity.GitHubLogin != "" {
		details = append(details, "github_login "+identity.GitHubLogin)
	}
	switch identity.OwnershipMode {
	case workflowconfig.IdentityOwnershipField:
		details = append(details, "owner field "+identity.OwnerField)
	default:
		details = append(details, "owner "+identity.OwnershipMode)
	}
	return strings.Join(details, ", ")
}

func doctorAuthorizationDetail(project globalconfig.Project, cfg workflowconfig.Config) string {
	projectAuthorization := project.Authorization.Configured()
	workflowAuthorization := cfg.Tracker.Authorization.Configured()
	switch {
	case projectAuthorization && workflowAuthorization:
		return "authorization selectors from global.yaml and WORKFLOW.md"
	case projectAuthorization:
		return "authorization selector from global.yaml"
	case workflowAuthorization:
		return "authorization selector from WORKFLOW.md"
	default:
		return "authorization allows all issues"
	}
}

func projectSourceRoot(project globalconfig.Project, cfg workflowconfig.Config) string {
	if sourceRoot := strings.TrimSpace(cfg.Workspace.SourceRoot); sourceRoot != "" {
		return sourceRoot
	}
	if workdir := strings.TrimSpace(project.Workdir); workdir != "" {
		return workdir
	}
	if root := strings.TrimSpace(cfg.Workspace.Root); root != "" {
		return root
	}
	return ""
}

func expandDoctorWorkspacePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Clean(home), nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func checkDoctorSQLite(ctx context.Context, resolution globalconfig.PathResolution, deps doctorDeps) doctorCheck {
	if strings.TrimSpace(resolution.Path) == "" {
		return doctorCheck{
			Name:   "SQLite database",
			Status: doctorFail,
			Detail: "global config path is unavailable",
			Hint:   "Fix config resolution, then rerun detent doctor.",
		}
	}

	dbPath := filepath.Join(filepath.Dir(resolution.Path), "detent.db")
	db, err := deps.openSQLite(ctx, dbPath)
	if err != nil {
		return doctorCheck{
			Name:   "SQLite database",
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", dbPath, err),
			Hint:   "Check directory permissions and remove any corrupt runtime database.",
		}
	}
	if err := db.Close(); err != nil {
		return doctorCheck{
			Name:   "SQLite database",
			Status: doctorFail,
			Detail: fmt.Sprintf("%s close failed: %v", dbPath, err),
			Hint:   "Check for filesystem or SQLite errors, then rerun detent doctor.",
		}
	}

	return doctorCheck{
		Name:   "SQLite database",
		Status: doctorOK,
		Detail: dbPath + " is reachable",
	}
}

func checkDoctorCodex(ctx context.Context, deps doctorDeps) doctorCheck {
	return checkDoctorBinary(ctx, deps, "codex", "codex binary", "--version", "Install Codex and ensure codex --version succeeds.")
}

func checkDoctorGit(ctx context.Context, deps doctorDeps) doctorCheck {
	return checkDoctorBinary(ctx, deps, "git", "git binary", "--version", "Install git and ensure git --version succeeds.")
}

func checkDoctorBinary(ctx context.Context, deps doctorDeps, binary string, name string, arg string, hint string) doctorCheck {
	path, err := deps.lookPath(binary)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: binary + " was not found on PATH",
			Hint:   hint,
		}
	}
	if err := deps.runCommand(ctx, path, arg); err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s %s failed: %v", path, arg, err),
			Hint:   hint,
		}
	}

	return doctorCheck{
		Name:   name,
		Status: doctorOK,
		Detail: path + " is runnable",
	}
}

func checkDoctorGitHub(ctx context.Context, cfg *globalconfig.Config, token RuntimeSecret, deps doctorDeps) doctorCheck {
	hasGitHubProject := doctorHasGitHubProject(cfg, deps)
	if cfg != nil && !hasGitHubProject {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorWarn,
			Detail: "no GitHub tracker projects configured; token scope check skipped",
			Hint:   "Add a GitHub project before relying on GitHub token preflight checks.",
		}
	}
	if token.Value == "" {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorFail,
			Detail: "GITHUB_TOKEN is not set, github_token is not configured, and no usable tracker.api_key was found",
			Hint:   githubAuthHint,
		}
	}

	source := doctorGitHubTokenSource(token)
	scopes, err := deps.githubScopes(ctx, token.Value)
	if err != nil {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorFail,
			Detail: fmt.Sprintf("%s scope check failed: %v", source, err),
			Hint:   `Refresh the token with repo, read:org, and project scopes.`,
		}
	}
	missing := missingGitHubScopes(scopes)
	if len(missing) > 0 {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorFail,
			Detail: fmt.Sprintf("%s missing scope(s): %s", source, strings.Join(missing, ", ")),
			Hint:   githubAuthHint,
		}
	}

	return doctorCheck{
		Name:   "GitHub token",
		Status: doctorOK,
		Detail: fmt.Sprintf("%s has required scopes: %s", source, strings.Join(requiredGitHubScopes, ", ")),
	}
}

func doctorHasGitHubProject(cfg *globalconfig.Config, deps doctorDeps) bool {
	if cfg != nil {
		for _, project := range cfg.Projects {
			workflow, err := deps.loadWorkflow(project.Workflow)
			if err != nil || workflow.Config.Tracker.Kind != workflowconfig.TrackerGitHub {
				continue
			}
			return true
		}
	}
	return false
}

func doctorGitHubTokenSource(token RuntimeSecret) string {
	if token.ResolvedVia == "gh" {
		return "github_token resolved via gh"
	}
	if source := strings.TrimSpace(token.Source); source != "" {
		return source
	}
	return "github_token"
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			continue
		}
		if index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func missingGitHubScopes(scopes []string) []string {
	have := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope != "" {
			have[scope] = struct{}{}
		}
	}

	var missing []string
	for _, scope := range requiredGitHubScopes {
		if _, ok := have[scope]; !ok {
			missing = append(missing, scope)
		}
	}
	return missing
}

func checkDoctorServerPort(cfg BootConfig, deps doctorDeps) doctorCheck {
	addr := serverAddr(cfg)
	listener, err := deps.listen("tcp", addr)
	if err != nil {
		return doctorCheck{
			Name:   "Server port",
			Status: doctorFail,
			Detail: fmt.Sprintf("%s is not available: %v", addr, err),
			Hint:   "Stop the process using the port or pass --port with an available value.",
		}
	}
	if err := listener.Close(); err != nil {
		return doctorCheck{
			Name:   "Server port",
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s was available, but close failed: %v", addr, err),
			Hint:   "Rerun detent doctor and check for local network errors.",
		}
	}

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err == nil && portText != "" {
		if port, parseErr := strconv.Atoi(portText); parseErr == nil && port > 0 && host != "" {
			addr = net.JoinHostPort(host, strconv.Itoa(port))
		}
	}

	return doctorCheck{
		Name:   "Server port",
		Status: doctorOK,
		Detail: addr + " is available",
	}
}

func doctorOptions(opts options) options {
	defaults := defaultOptions()
	if opts.resolvePath == nil {
		opts.resolvePath = defaults.resolvePath
	}
	if opts.read == nil {
		opts.read = defaults.read
	}
	return opts
}

func (d doctorDeps) withDefaults() doctorDeps {
	defaults := defaultDoctorDeps()
	if d.loadWorkflow == nil {
		d.loadWorkflow = defaults.loadWorkflow
	}
	if d.lookupEnv == nil {
		d.lookupEnv = defaults.lookupEnv
	}
	if d.lookPath == nil {
		d.lookPath = defaults.lookPath
	}
	if d.runCommand == nil {
		d.runCommand = defaults.runCommand
	}
	if d.githubScopes == nil {
		d.githubScopes = defaults.githubScopes
	}
	if d.ghAuthToken == nil {
		d.ghAuthToken = defaults.ghAuthToken
	}
	if d.listen == nil {
		d.listen = defaults.listen
	}
	if d.openSQLite == nil {
		d.openSQLite = defaults.openSQLite
	}
	if d.gitWorkTree == nil {
		d.gitWorkTree = defaults.gitWorkTree
	}
	return d
}

func defaultDoctorDeps() doctorDeps {
	return doctorDeps{
		loadWorkflow: workflowconfig.LoadWorkflow,
		lookupEnv:    os.Getenv,
		lookPath:     exec.LookPath,
		runCommand:   runDoctorCommand,
		githubScopes: defaultGitHubScopes,
		ghAuthToken:  defaultGHAuthToken,
		listen:       net.Listen,
		openSQLite:   openDoctorSQLite,
		gitWorkTree:  defaultGitWorkTree,
	}
}

func openDoctorSQLite(ctx context.Context, path string) (doctorStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		return nil, doctorSQLitePingError(err, db.Close())
	}
	return db, nil
}

func doctorSQLitePingError(err, closeErr error) error {
	if closeErr != nil {
		return fmt.Errorf("ping sqlite database: %w; close sqlite database: %w", err, closeErr)
	}
	return fmt.Errorf("ping sqlite database: %w", err)
}

func runDoctorCommand(ctx context.Context, path string, args ...string) error {
	commandCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, path, args...) // #nosec G204 -- doctor runs fixed PATH-resolved preflight binaries.
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return commandCtx.Err()
	}
	if err == nil {
		return nil
	}
	if detail := strings.TrimSpace(string(output)); detail != "" {
		return fmt.Errorf("%w: %s", err, detail)
	}
	return err
}

func defaultGitWorkTree(ctx context.Context, path string) error {
	commandCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, "git", "-C", path, "rev-parse", "--is-inside-work-tree")
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return commandCtx.Err()
	}
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return fmt.Errorf("%w: %s", err, detail)
		}
		return err
	}
	if strings.TrimSpace(string(output)) != "true" {
		return errors.New("not inside a git worktree")
	}
	return nil
}

func defaultGitHubScopes(ctx context.Context, token string) ([]string, error) {
	requestCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "detent-doctor")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}

	scopes := parseGitHubScopes(resp.Header.Get("X-OAuth-Scopes"))
	if len(scopes) == 0 {
		return nil, errors.New("GitHub did not report OAuth scopes")
	}
	return scopes, nil
}

func parseGitHubScopes(raw string) []string {
	fields := strings.Split(raw, ",")
	scopes := make([]string, 0, len(fields))
	for _, field := range fields {
		scope := strings.TrimSpace(field)
		if scope != "" {
			scopes = append(scopes, scope)
		}
	}
	sort.Strings(scopes)
	return scopes
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefInt(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}
