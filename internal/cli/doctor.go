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
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

var ErrDoctorFailed = errors.New("doctor found failed checks")

const (
	doctorCommandTimeout = 5 * time.Second
	doctorCheckTimeout   = 30 * time.Second
)

type doctorStatus string

const (
	doctorOK   doctorStatus = "OK"
	doctorWarn doctorStatus = "WARN"
	doctorFail doctorStatus = "FAIL"
)

type doctorCheck struct {
	Name                      string                                     `json:"name"`
	Status                    doctorStatus                               `json:"status"`
	Detail                    string                                     `json:"detail"`
	Hint                      string                                     `json:"hint,omitempty"`
	AutoPromoteCandidates     []doctorAutoPromoteCandidateDiagnostic     `json:"auto_promote_candidates,omitempty"`
	BlockedRecoveryCandidates []doctorBlockedRecoveryCandidateDiagnostic `json:"blocked_recovery_candidates,omitempty"`
	BackendCapacity           []doctorBackendCapacityDiagnostic          `json:"backend_capacity,omitempty"`
	DependencyCapabilities    []connector.DependencyCapability           `json:"dependency_capabilities,omitempty"`
	UntrackedIssues           []doctorStatusDriftIssueDiagnostic         `json:"untracked_issues,omitempty"`
	OpenTerminalIssues        []doctorStatusDriftIssueDiagnostic         `json:"open_terminal_issues,omitempty"`
	WorkflowOptimization      doctorWorkflowOptimizationReport           `json:"-"`
}

type doctorReport struct {
	Checks               []doctorCheck                    `json:"checks"`
	Scope                doctorScope                      `json:"scope,omitzero"`
	WorkflowOptimization doctorWorkflowOptimizationReport `json:"workflow_optimization"`
	Summary              doctorSummary                    `json:"summary"`
	Result               string                           `json:"result"`
	strict               bool
}

type doctorScope struct {
	SelectedProject string   `json:"selected_project,omitempty"`
	SkippedProjects []string `json:"skipped_projects,omitempty"`
}

type doctorSummary struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

type doctorOutputReport struct {
	Checks               []doctorCheck                    `json:"checks"`
	Scope                doctorScope                      `json:"scope,omitzero"`
	WorkflowOptimization doctorWorkflowOptimizationReport `json:"workflow_optimization"`
	Summary              doctorSummary                    `json:"summary"`
	Result               string                           `json:"result"`
}

type doctorCheckJob struct {
	Name    string
	Current func() string
	Run     func(context.Context) []doctorCheck
}

type doctorCheckResult struct {
	Index  int
	Checks []doctorCheck
}

type doctorCheckProgress struct {
	mu      sync.Mutex
	current string
}

func (p *doctorCheckProgress) Set(current string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current = strings.TrimSpace(current)
}

func (p *doctorCheckProgress) Current() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

type doctorConfig struct {
	ConfigPath                string
	Host                      string
	ProjectID                 string
	Flags                     runtimeFlags
	Output                    io.Writer
	CheckTimeout              time.Duration
	Build                     buildinfo.Info
	AllowWriteProbes          bool
	WorkflowDiff              bool
	WorkflowProposalThreshold int
	WorkflowProposeIssues     bool
}

type doctorStore interface {
	Close() error
}

type doctorTelemetryStore interface {
	doctorStore
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type doctorDeps struct {
	loadWorkflow         func(string) (workflowconfig.Workflow, error)
	lookupEnv            func(string) string
	lookPath             func(string) (string, error)
	runCommand           func(context.Context, string, ...string) error
	httpDo               func(*http.Request) (*http.Response, error)
	githubScopes         func(context.Context, string) ([]string, error)
	githubReadiness      doctorGitHubReadinessFunc
	ghAuthToken          func(context.Context) (string, error)
	listen               func(string, string) (net.Listener, error)
	openSQLite           func(context.Context, string) (doctorStore, error)
	openSQLiteReadOnly   func(context.Context, string) (doctorTelemetryStore, error)
	gitWorkTree          func(context.Context, string) error
	gitRemoteURL         func(context.Context, string) (string, error)
	autoPromoteConnector func(workflowconfig.Config) (doctorAutoPromoteConnector, error)
	proposalConnector    func(workflowconfig.Config) (doctorWorkflowProposalConnector, error)
	modelProbe           func(context.Context, doctorRouteModelProbeRequest) error
	executable           func() (string, error)
}

func newDoctorCommand(configPath *string, env *string, logLevel *string, host *string, port *int, opts options) *cobra.Command {
	return newDoctorCommandWithDeps(configPath, env, logLevel, host, port, opts, doctorDeps{})
}

func newDoctorCommandWithDeps(configPath *string, env *string, logLevel *string, host *string, port *int, opts options, deps doctorDeps) *cobra.Command {
	timeout := doctorCheckTimeout
	allowWriteProbes := false
	workflowDiff := false
	workflowWrite := false
	workflowProposeIssues := false
	workflowProposalThreshold := doctorWorkflowProposalDefaultThreshold
	strict := false
	projectID := ""
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Run preflight health checks",
		Example:      "detent doctor --config ~/.config/detent/global.yaml\n  detent doctor --project example --port 0",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			progressOut := cmd.OutOrStdout()
			if out.IsJSON() {
				progressOut = cmd.ErrOrStderr()
			}
			report := runDoctor(cmd.Context(), doctorConfig{
				ConfigPath:                derefString(configPath),
				Host:                      derefString(host),
				ProjectID:                 projectID,
				Output:                    progressOut,
				CheckTimeout:              timeout,
				Build:                     opts.build,
				AllowWriteProbes:          allowWriteProbes,
				WorkflowDiff:              workflowDiff || workflowWrite,
				WorkflowProposalThreshold: workflowProposalThreshold,
				WorkflowProposeIssues:     workflowProposeIssues,
				Flags: runtimeFlags{
					Env:      runtimeStringFlag{Value: derefString(env), Set: flagChanged(cmd, "env")},
					LogLevel: runtimeStringFlag{Value: derefString(logLevel), Set: flagChanged(cmd, "log-level")},
					Port:     runtimeIntFlag{Value: derefInt(port, -1), Set: flagChanged(cmd, "port")},
				},
			}, opts, deps)
			report.strict = strict
			if workflowWrite {
				if err := confirmDoctorWorkflowOptimizationWrite(cmd, report.WorkflowOptimization); err != nil {
					return err
				}
				written, err := writeDoctorWorkflowOptimizationPatches(report.WorkflowOptimization)
				if err != nil {
					return err
				}
				report.WorkflowOptimization.Written = written
			}
			if err := out.Write(func(out io.Writer) error {
				return writeDoctorReport(out, report)
			}, newDoctorOutputReport(report)); err != nil {
				return err
			}
			if report.hasExitFailure() {
				return ErrDoctorFailed
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", doctorCheckTimeout, "per-check timeout")
	cmd.Flags().BoolVar(&allowWriteProbes, "allow-write-probes", false, "run configured GitHub write probes")
	cmd.Flags().BoolVar(&workflowDiff, "diff", false, "print proposed WORKFLOW.md frontmatter changes for workflow optimization findings")
	cmd.Flags().BoolVar(&workflowWrite, "write", false, "apply proposed WORKFLOW.md frontmatter changes after confirmation")
	cmd.Flags().IntVar(&workflowProposalThreshold, "proposal-threshold", doctorWorkflowProposalDefaultThreshold, "minimum repeated signal count before emitting a governed self-improvement proposal")
	cmd.Flags().BoolVar(&workflowProposeIssues, "propose-issues", false, "create governed backlog issue proposals for repeated workflow optimization signals")
	cmd.Flags().BoolVar(&strict, "strict", false, "fail when checks warn or workflow optimization findings exist")
	cmd.Flags().StringVar(&projectID, "project", "", "limit project checks to the selected project id")
	cmd.SetContext(withCommandOutputOptions(context.Background(), commandOutputOptions{
		lookupEnv: opts.lookupEnv,
		stdoutTTY: opts.stdoutTTY,
	}))
	return cmd
}

func runDoctor(ctx context.Context, cfg doctorConfig, opts options, deps doctorDeps) doctorReport {
	if ctx == nil {
		ctx = context.Background()
	}
	opts = doctorOptions(opts)
	deps = deps.withDefaults()
	timeout := doctorNormalizedTimeout(cfg.CheckTimeout)
	progressOut := cfg.Output

	report := doctorReport{Scope: doctorScope{SelectedProject: strings.TrimSpace(cfg.ProjectID)}}
	writeDoctorProgressStart(progressOut, "Config resolution")
	resolution, global, scope, projectScopeCheck, configCheck := checkDoctorConfig(cfg.ConfigPath, cfg.ProjectID, opts)
	writeDoctorProgressDone(progressOut, configCheck)
	report.Scope = scope
	report.Add(configCheck)

	if projectScopeCheck != nil {
		writeDoctorProgressStart(progressOut, projectScopeCheck.Name)
		writeDoctorProgressDone(progressOut, *projectScopeCheck)
		report.Add(*projectScopeCheck)
	}

	workflowPath := ""
	if global != nil {
		workflowPath = firstGlobalWorkflowPath(*global)
	}
	writeDoctorProgressStart(progressOut, "Runtime settings")
	runtimeCtx, cancelRuntime := context.WithTimeout(ctx, timeout)
	runtime, runtimeErr := resolveRuntimeSettings(runtimeCtx, runtimeInput{
		Config:     global,
		ConfigPath: resolution,
		Workflow:   workflowPath,
		Flags:      cfg.Flags,
	}, runtimeDeps{
		lookupEnv:    deps.lookupEnv,
		ghAuthToken:  deps.ghAuthToken,
		loadWorkflow: deps.loadWorkflow,
	})
	cancelRuntime()
	if runtimeErr != nil {
		hint := "Fix runtime flags, environment variables, or global.yaml."
		if runtimeHint, _, ok := HintFor(runtimeErr); ok && strings.TrimSpace(runtimeHint) != "" {
			hint = runtimeHint
		}
		check := doctorCheck{
			Name:   "Runtime settings",
			Status: doctorFail,
			Detail: runtimeErr.Error(),
			Hint:   hint,
		}
		writeDoctorProgressDone(progressOut, check)
		report.Add(check)
	} else {
		check := checkDoctorRuntimeSettings(runtime)
		writeDoctorProgressDone(progressOut, check)
		report.Add(check)
	}
	writeDoctorProgressStart(progressOut, "Detent executable")
	executableCheck := checkDoctorDetentExecutable(cfg.Build, deps)
	writeDoctorProgressDone(progressOut, executableCheck)
	report.Add(executableCheck)

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
		boot.Host = bootHost(ctx, cfg.Host, firstGlobalProject(*global))
		writeDoctorProgressStart(progressOut, "Global config reload")
		check := checkDoctorConfigReload(*global)
		writeDoctorProgressDone(progressOut, check)
		report.Add(check)
		writeDoctorProgressStart(progressOut, "Instance identity")
		check = checkDoctorInstanceIdentity(*global)
		writeDoctorProgressDone(progressOut, check)
		report.Add(check)
	} else {
		writeDoctorProgressStart(progressOut, "Project workflows")
		check := doctorCheck{
			Name:   "Project workflows",
			Status: doctorWarn,
			Detail: "skipped because global config could not be loaded",
			Hint:   "Fix the global config, then rerun detent doctor.",
		}
		writeDoctorProgressDone(progressOut, check)
		report.Add(check)
	}

	jobs := []doctorCheckJob{}
	if global != nil {
		globalConfig := *global
		githubToken := runtime.GitHubToken
		if projectScopeCheck == nil {
			jobs = append(jobs, doctorProjectCheckJobs(globalConfig, deps, githubToken, cfg.AllowWriteProbes)...)
		}
		jobs = append(jobs, doctorCheckJob{
			Name: "Workflow optimization",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorWorkflowOptimization(jobCtx, resolution, globalConfig, deps, githubToken, doctorWorkflowOptimizationOptions{
					IncludeDiff:       cfg.WorkflowDiff,
					ProposalThreshold: cfg.WorkflowProposalThreshold,
					ProposeIssues:     cfg.WorkflowProposeIssues,
				})}
			},
		})
	}
	jobs = append(jobs,
		doctorCheckJob{
			Name: "SQLite database",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorSQLite(jobCtx, resolution, deps)}
			},
		},
		doctorCheckJob{
			Name: "Backend capacity",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorBackendCapacity(jobCtx, resolution, cfg.ProjectID, deps, time.Now())}
			},
		},
	)
	jobs = append(jobs, doctorAgentBinaryCheckJobs(ctx, global, deps)...)
	jobs = append(jobs,
		doctorCheckJob{
			Name: "GitHub token",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorGitHub(jobCtx, global, runtime.GitHubToken, deps)}
			},
		},
		doctorCheckJob{
			Name: "Server port",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorServerPort(jobCtx, boot, deps)}
			},
		},
		doctorCheckJob{
			Name: "git binary",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorGit(jobCtx, deps)}
			},
		},
	)
	for _, checks := range runDoctorChecks(ctx, jobs, timeout, progressOut) {
		for _, check := range checks {
			report.Add(check)
		}
	}
	if cfg.WorkflowDiff {
		report.WorkflowOptimization.Diff = doctorWorkflowOptimizationDiff(report.WorkflowOptimization)
	}

	return report
}

func runDoctorChecks(ctx context.Context, jobs []doctorCheckJob, timeout time.Duration, out io.Writer) [][]doctorCheck {
	results := make([][]doctorCheck, len(jobs))
	done := make(chan doctorCheckResult, len(jobs))
	for i, job := range jobs {
		writeDoctorProgressStart(out, job.Name)
		go func(index int, job doctorCheckJob) {
			done <- doctorCheckResult{
				Index:  index,
				Checks: runDoctorCheck(ctx, job, timeout),
			}
		}(i, job)
	}
	for range jobs {
		result := <-done
		for _, check := range result.Checks {
			writeDoctorProgressDone(out, check)
		}
		results[result.Index] = result.Checks
	}
	return results
}

func runDoctorCheck(ctx context.Context, job doctorCheckJob, timeout time.Duration) []doctorCheck {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout = doctorNormalizedTimeout(timeout)
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan []doctorCheck, 1)
	go func() {
		done <- job.Run(checkCtx)
	}()

	select {
	case checks := <-done:
		return checks
	case <-checkCtx.Done():
		return []doctorCheck{{
			Name:   job.Name,
			Status: doctorFail,
			Detail: doctorTimeoutDetail(job, timeout, checkCtx.Err()),
			Hint:   doctorTimeoutHint(),
		}}
	}
}

func doctorTimeoutDetail(job doctorCheckJob, timeout time.Duration, err error) string {
	current := ""
	if job.Current != nil {
		current = strings.TrimSpace(job.Current())
	}
	if current != "" && current != strings.TrimSpace(job.Name) {
		return fmt.Sprintf("timed out after %s while running %s: %v", timeout, current, err)
	}
	return fmt.Sprintf("timed out after %s: %v", timeout, err)
}

func doctorTimeoutHint() string {
	return "Rerun detent doctor --timeout 30s --port 0; if this repeats, check network access, GitHub availability, local subprocesses, and SQLite locks."
}

func doctorNormalizedTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return doctorCheckTimeout
	}
	return timeout
}

func writeDoctorProgressStart(out io.Writer, name string) {
	if out == nil {
		return
	}
	fmt.Fprintf(out, "%-5s  %-28s  checking\n", "RUN", name)
}

func writeDoctorProgressDone(out io.Writer, check doctorCheck) {
	if out == nil {
		return
	}
	fmt.Fprintf(out, "%-5s  %-28s  %s\n", check.Status, check.Name, check.Detail)
}

func (r *doctorReport) Add(check doctorCheck) {
	r.WorkflowOptimization.Merge(check.WorkflowOptimization)
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

func (r doctorReport) HasStrictFailures() bool {
	for _, check := range r.Checks {
		if check.Status == doctorFail || check.Status == doctorWarn {
			return true
		}
	}
	return len(r.WorkflowOptimization.Findings) > 0
}

func (r doctorReport) hasExitFailure() bool {
	if r.strict {
		return r.HasStrictFailures()
	}
	return r.HasFailures()
}

func (r doctorReport) result() string {
	if r.hasExitFailure() {
		return "FAIL"
	}
	return "PASS"
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

func (r doctorReport) withSummary() doctorReport {
	counts := r.counts()
	r.Summary = doctorSummary{
		OK:   counts[doctorOK],
		Warn: counts[doctorWarn],
		Fail: counts[doctorFail],
	}
	r.Result = r.result()
	return r
}

func scopeDoctorGlobalConfig(cfg globalconfig.Config, projectID string) (globalconfig.Config, doctorScope, *doctorCheck) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return cfg, doctorScope{}, nil
	}

	scope := doctorScope{SelectedProject: projectID}
	scoped := cfg
	scoped.Projects = nil
	for _, project := range cfg.Projects {
		id := doctorProjectID(project)
		if id == projectID {
			scoped.Projects = append(scoped.Projects, project)
			continue
		}
		scope.SkippedProjects = append(scope.SkippedProjects, id)
	}
	if len(scoped.Projects) > 0 {
		return scoped, scope, nil
	}

	check := missingDoctorProjectScopeCheck(scope)
	return scoped, scope, &check
}

func writeDoctorReport(out io.Writer, report doctorReport, format ...OutputFormat) error {
	if out == nil {
		out = io.Discard
	}
	if len(format) > 0 && format[0] == OutputFormatJSON {
		return WriteJSON(out, report.withSummary())
	}

	if _, err := fmt.Fprintln(out, "Detent Doctor"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	if report.Scope.SelectedProject != "" {
		if _, err := fmt.Fprintf(out, "Scope: project %s", report.Scope.SelectedProject); err != nil {
			return err
		}
		if len(report.Scope.SkippedProjects) > 0 {
			if _, err := fmt.Fprintf(out, " (skipped: %s)", strings.Join(report.Scope.SkippedProjects, ", ")); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
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
	if err := writeDoctorWorkflowOptimizationPretty(out, report.WorkflowOptimization); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	report = report.withSummary()
	if _, err := fmt.Fprintf(out, "Summary: %d OK, %d WARN, %d FAIL\n", report.Summary.OK, report.Summary.Warn, report.Summary.Fail); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "Result: %s\n", report.Result)
	return err
}

func newDoctorOutputReport(report doctorReport) doctorOutputReport {
	counts := report.counts()
	return doctorOutputReport{
		Checks:               report.Checks,
		Scope:                report.Scope,
		WorkflowOptimization: report.WorkflowOptimization,
		Summary: doctorSummary{
			OK:   counts[doctorOK],
			Warn: counts[doctorWarn],
			Fail: counts[doctorFail],
		},
		Result: report.result(),
	}
}

func checkDoctorConfig(configPath string, projectID string, opts options) (globalconfig.PathResolution, *globalconfig.Config, doctorScope, *doctorCheck, doctorCheck) {
	projectID = strings.TrimSpace(projectID)
	scope := doctorScope{SelectedProject: projectID}
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return globalconfig.PathResolution{}, nil, scope, nil, doctorCheck{
			Name:   "Config resolution",
			Status: doctorFail,
			Detail: err.Error(),
			Hint:   "Pass --config or set CONFIG to a readable global.yaml.",
		}
	}

	var cfg globalconfig.Config
	if projectID == "" {
		cfg, err = opts.read(resolution.Path)
	} else {
		var skippedProjects []string
		cfg, skippedProjects, err = opts.readProject(resolution.Path, projectID)
		scope.SkippedProjects = skippedProjects
	}
	if err != nil {
		return resolution, nil, scope, nil, doctorCheck{
			Name:   "Config resolution",
			Status: doctorFail,
			Detail: fmt.Sprintf("%s via %s; %v", resolution.Path, resolution.Rule, err),
			Hint:   "Run detent init or fix the global config file.",
		}
	}

	var projectScopeCheck *doctorCheck
	if projectID != "" && len(cfg.Projects) == 0 {
		check := missingDoctorProjectScopeCheck(scope)
		projectScopeCheck = &check
	}

	return resolution, &cfg, scope, projectScopeCheck, doctorCheck{
		Name:   "Config resolution",
		Status: doctorOK,
		Detail: fmt.Sprintf("%s via %s; %d project(s)", cfg.Path, resolution.Rule, len(cfg.Projects)),
	}
}

func missingDoctorProjectScopeCheck(scope doctorScope) doctorCheck {
	detail := "project " + scope.SelectedProject + " not found"
	if len(scope.SkippedProjects) > 0 {
		detail += "; configured projects: " + strings.Join(scope.SkippedProjects, ", ")
	}
	return doctorCheck{
		Name:   "Project scope",
		Status: doctorFail,
		Detail: detail,
		Hint:   "Run detent doctor without --project to list host-wide project checks, or pass a configured project id.",
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

func checkDoctorDetentExecutable(build buildinfo.Info, deps doctorDeps) doctorCheck {
	path, err := deps.executable()
	if err != nil {
		return doctorCheck{
			Name:   "Detent executable",
			Status: doctorFail,
			Detail: err.Error(),
			Hint:   "Start Detent from the expected installed binary.",
		}
	}
	path = filepath.Clean(path)
	detail := path + " is running"
	if !buildinfo.IsZero(build) {
		detail = path + " " + buildinfo.DisplayLabel(build)
	}
	return doctorCheck{
		Name:   "Detent executable",
		Status: doctorOK,
		Detail: detail,
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
	if opts.readProject == nil {
		opts.readProject = defaults.readProject
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
	if d.httpDo == nil {
		d.httpDo = defaults.httpDo
	}
	if d.githubScopes == nil {
		d.githubScopes = defaults.githubScopes
	}
	if d.githubReadiness == nil {
		d.githubReadiness = defaults.githubReadiness
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
	if d.openSQLiteReadOnly == nil {
		d.openSQLiteReadOnly = defaults.openSQLiteReadOnly
	}
	if d.gitWorkTree == nil {
		d.gitWorkTree = defaults.gitWorkTree
	}
	if d.gitRemoteURL == nil {
		d.gitRemoteURL = defaults.gitRemoteURL
	}
	if d.autoPromoteConnector == nil {
		d.autoPromoteConnector = defaults.autoPromoteConnector
	}
	if d.proposalConnector == nil {
		d.proposalConnector = defaults.proposalConnector
	}
	if d.modelProbe == nil {
		d.modelProbe = defaults.modelProbe
	}
	if d.executable == nil {
		d.executable = defaults.executable
	}
	return d
}

func defaultDoctorDeps() doctorDeps {
	return doctorDeps{
		loadWorkflow:         workflowconfig.LoadWorkflow,
		lookupEnv:            os.Getenv,
		lookPath:             exec.LookPath,
		runCommand:           runDoctorCommand,
		httpDo:               defaultDoctorHTTPDo,
		githubScopes:         defaultGitHubScopes,
		githubReadiness:      ghconnector.CheckReadiness,
		ghAuthToken:          defaultGHAuthToken,
		listen:               net.Listen,
		openSQLite:           openDoctorSQLite,
		openSQLiteReadOnly:   openDoctorSQLiteReadOnly,
		gitWorkTree:          defaultGitWorkTree,
		gitRemoteURL:         defaultGitRemoteURL,
		autoPromoteConnector: defaultDoctorAutoPromoteConnector,
		proposalConnector:    defaultDoctorProposalConnector,
		modelProbe:           defaultDoctorRouteModelProbe,
		executable:           os.Executable,
	}
}

func defaultDoctorHTTPDo(req *http.Request) (*http.Response, error) {
	client := http.Client{Timeout: doctorCommandTimeout}
	return client.Do(req)
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

func defaultGitRemoteURL(ctx context.Context, path string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, "git", "-C", path, "remote", "get-url", "origin") // #nosec G204 -- doctor runs fixed git preflight arguments against configured checkout paths.
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return "", commandCtx.Err()
	}
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	remote := strings.TrimSpace(string(output))
	if remote == "" {
		return "", errors.New("origin remote URL is blank")
	}
	return remote, nil
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
