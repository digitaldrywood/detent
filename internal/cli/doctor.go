package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/factory"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
)

var ErrDoctorFailed = errors.New("doctor found failed checks")

const (
	doctorCommandTimeout = 5 * time.Second
	doctorCheckTimeout   = 30 * time.Second
)

var (
	doctorDependencyIssueURLPattern = regexp.MustCompile(`https://github\.com/([^/\s]+/[^/\s]+)/(?:issues|pull)/(\d+)`)
	doctorDependencyIssueRefPattern = regexp.MustCompile(`(?:^|[\s(,;:])([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#(\d+)\b`)
)

type doctorStatus string

const (
	doctorOK   doctorStatus = "OK"
	doctorWarn doctorStatus = "WARN"
	doctorFail doctorStatus = "FAIL"
)

var (
	requiredProjectV2GitHubScopes  = []string{"repo", "read:org", "read:project", "project"}
	requiredIssueFieldGitHubScopes = []string{"repo", "read:org"}
	requiredLabelGitHubScopes      = []string{"repo"}
)

const (
	doctorAutoPromoteSampleLimit           = 5
	doctorDependencyAutoUnblockSampleLimit = 5
	doctorBlockedRecoverySampleLimit       = 5
)

var doctorHealthCheckKeys = []string{"hub", "store", "registry", "connector"}

type doctorCheck struct {
	Name                      string                                     `json:"name"`
	Status                    doctorStatus                               `json:"status"`
	Detail                    string                                     `json:"detail"`
	Hint                      string                                     `json:"hint,omitempty"`
	AutoPromoteCandidates     []doctorAutoPromoteCandidateDiagnostic     `json:"auto_promote_candidates,omitempty"`
	BlockedRecoveryCandidates []doctorBlockedRecoveryCandidateDiagnostic `json:"blocked_recovery_candidates,omitempty"`
}

type doctorReport struct {
	Checks  []doctorCheck `json:"checks"`
	Summary doctorSummary `json:"summary"`
	Result  string        `json:"result"`
}

type doctorAutoPromoteCandidateDiagnostic struct {
	IssueID                      string     `json:"issue_id,omitempty"`
	IssueIdentifier              string     `json:"issue_identifier,omitempty"`
	IssueURL                     string     `json:"issue_url,omitempty"`
	PRNumber                     int        `json:"pr_number,omitempty"`
	PRURL                        string     `json:"pr_url,omitempty"`
	PRHeadSHA                    string     `json:"pr_head_sha,omitempty"`
	PRMergeableState             string     `json:"pr_mergeable_state,omitempty"`
	LatestCodexReviewState       string     `json:"latest_codex_review_state,omitempty"`
	LatestCodexReviewCommitSHA   string     `json:"latest_codex_review_commit_sha,omitempty"`
	LatestCodexReviewSubmittedAt *time.Time `json:"latest_codex_review_submitted_at,omitempty"`
	QuietRemainingSeconds        int64      `json:"quiet_remaining_seconds,omitempty"`
	Reason                       string     `json:"reason"`
}

type doctorBlockedRecoveryCandidateDiagnostic struct {
	IssueID         string `json:"issue_id,omitempty"`
	IssueIdentifier string `json:"issue_identifier,omitempty"`
	IssueURL        string `json:"issue_url,omitempty"`
	PRNumber        int    `json:"pr_number,omitempty"`
	PRURL           string `json:"pr_url,omitempty"`
	PRHeadSHA       string `json:"pr_head_sha,omitempty"`
	TargetState     string `json:"target_state,omitempty"`
	Reason          string `json:"reason"`
	Detail          string `json:"detail,omitempty"`
}

type doctorSummary struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

type doctorOutputReport struct {
	Checks  []doctorCheck `json:"checks"`
	Summary doctorSummary `json:"summary"`
	Result  string        `json:"result"`
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
	ConfigPath   string
	Host         string
	Flags        runtimeFlags
	Output       io.Writer
	CheckTimeout time.Duration
	Build        buildinfo.Info
}

type doctorStore interface {
	Close() error
}

type doctorAutoPromoteConnector interface {
	FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error)
}

type doctorAutoPromoteLimitedConnector interface {
	FetchIssuesByStatesLimit(context.Context, []string, int) ([]connector.Issue, error)
}

type doctorStatusOptionVerifier interface {
	VerifyStatusOptions(context.Context, []string) error
}

type doctorGitHubReadinessFunc func(context.Context, ghconnector.Config, ghconnector.ReadinessConfig) ([]ghconnector.ReadinessCheck, error)

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
	gitWorkTree          func(context.Context, string) error
	gitRemoteURL         func(context.Context, string) (string, error)
	autoPromoteConnector func(workflowconfig.Config) (doctorAutoPromoteConnector, error)
	executable           func() (string, error)
}

func newDoctorCommand(configPath *string, env *string, logLevel *string, host *string, port *int, opts options) *cobra.Command {
	return newDoctorCommandWithDeps(configPath, env, logLevel, host, port, opts, doctorDeps{})
}

func newDoctorCommandWithDeps(configPath *string, env *string, logLevel *string, host *string, port *int, opts options, deps doctorDeps) *cobra.Command {
	timeout := doctorCheckTimeout
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Run preflight health checks",
		Example:      "detent doctor --config ~/.config/detent/global.yaml",
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
				ConfigPath:   derefString(configPath),
				Host:         derefString(host),
				Output:       progressOut,
				CheckTimeout: timeout,
				Build:        opts.build,
				Flags: runtimeFlags{
					Env:      runtimeStringFlag{Value: derefString(env), Set: flagChanged(cmd, "env")},
					LogLevel: runtimeStringFlag{Value: derefString(logLevel), Set: flagChanged(cmd, "log-level")},
					Port:     runtimeIntFlag{Value: derefInt(port, -1), Set: flagChanged(cmd, "port")},
				},
			}, opts, deps)
			if err := out.Write(func(out io.Writer) error {
				return writeDoctorReport(out, report)
			}, newDoctorOutputReport(report)); err != nil {
				return err
			}
			if report.HasFailures() {
				return ErrDoctorFailed
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", doctorCheckTimeout, "per-check timeout")
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

	var report doctorReport
	writeDoctorProgressStart(progressOut, "Config resolution")
	resolution, global, configCheck := checkDoctorConfig(cfg.ConfigPath, opts)
	writeDoctorProgressDone(progressOut, configCheck)
	report.Add(configCheck)

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
		jobs = append(jobs, doctorProjectCheckJobs(globalConfig, deps, githubToken)...)
	}
	jobs = append(jobs,
		doctorCheckJob{
			Name: "SQLite database",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorSQLite(jobCtx, resolution, deps)}
			},
		},
		doctorCheckJob{
			Name: "codex binary",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorCodex(jobCtx, deps)}
			},
		},
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
		report.Checks = append(report.Checks, checks...)
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

func (r doctorReport) withSummary() doctorReport {
	counts := r.counts()
	r.Summary = doctorSummary{
		OK:   counts[doctorOK],
		Warn: counts[doctorWarn],
		Fail: counts[doctorFail],
	}
	r.Result = "PASS"
	if r.Summary.Fail > 0 {
		r.Result = "FAIL"
	}
	return r
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
	result := "PASS"
	if counts[doctorFail] > 0 {
		result = "FAIL"
	}
	return doctorOutputReport{
		Checks: report.Checks,
		Summary: doctorSummary{
			OK:   counts[doctorOK],
			Warn: counts[doctorWarn],
			Fail: counts[doctorFail],
		},
		Result: result,
	}
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

func checkDoctorProjects(ctx context.Context, cfg globalconfig.Config, deps doctorDeps, githubToken RuntimeSecret) []doctorCheck {
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
		checks = append(checks, checkDoctorProject(ctx, project, deps, githubToken)...)
	}

	return checks
}

func doctorProjectCheckJobs(cfg globalconfig.Config, deps doctorDeps, githubToken RuntimeSecret) []doctorCheckJob {
	if len(cfg.Projects) == 0 {
		return []doctorCheckJob{{
			Name: "Project workflows",
			Run: func(context.Context) []doctorCheck {
				return []doctorCheck{
					{
						Name:   "Project workflows",
						Status: doctorWarn,
						Detail: "no projects configured",
						Hint:   "Run detent add-project to add a project.",
					},
				}
			},
		}}
	}

	jobs := make([]doctorCheckJob, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		id := doctorProjectID(project)
		progress := &doctorCheckProgress{}
		jobs = append(jobs, doctorCheckJob{
			Name:    "Project " + id + " checks",
			Current: progress.Current,
			Run: func(jobCtx context.Context) []doctorCheck {
				return checkDoctorProjectWithProgress(jobCtx, project, deps, githubToken, progress.Set)
			},
		})
	}
	return jobs
}

func checkDoctorProject(ctx context.Context, project globalconfig.Project, deps doctorDeps, githubToken RuntimeSecret) []doctorCheck {
	return checkDoctorProjectWithProgress(ctx, project, deps, githubToken, nil)
}

func checkDoctorProjectWithProgress(
	ctx context.Context,
	project globalconfig.Project,
	deps doctorDeps,
	githubToken RuntimeSecret,
	setCurrent func(string),
) []doctorCheck {
	id := doctorProjectID(project)
	setDoctorCurrentCheck := func(name string) {
		if setCurrent != nil {
			setCurrent(name)
		}
	}
	workflowCheckName := "Project " + id + " workflow"
	setDoctorCurrentCheck(workflowCheckName)
	workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
	if err != nil {
		return []doctorCheck{
			{
				Name:   workflowCheckName,
				Status: doctorFail,
				Detail: fmt.Sprintf("%s: %v", project.Workflow, err),
				Hint:   "Fix the WORKFLOW.md path or YAML frontmatter.",
			},
			{
				Name:   "Project " + id + " source repo",
				Status: doctorWarn,
				Detail: "skipped because WORKFLOW.md could not be loaded",
				Hint:   "Fix the workflow file, then rerun detent doctor.",
			},
		}
	}
	workflow.Config = doctorWorkflowConfigWithRuntimeGitHubToken(workflow.Config, runtimeGlobalGitHubToken(githubToken))
	if err := workflow.Config.Validate(); err != nil {
		return []doctorCheck{
			{
				Name:   workflowCheckName,
				Status: doctorFail,
				Detail: fmt.Sprintf("%s: %v", project.Workflow, err),
				Hint:   "Fix invalid WORKFLOW.md frontmatter.",
			},
			{
				Name:   "Project " + id + " source repo",
				Status: doctorWarn,
				Detail: "skipped because WORKFLOW.md is invalid",
				Hint:   "Fix the workflow file, then rerun detent doctor.",
			},
		}
	}

	checks := []doctorCheck{
		{
			Name:   workflowCheckName,
			Status: doctorOK,
			Detail: doctorWorkflowDetail(project.Workflow, project, workflow.Config),
		},
	}
	if workflow.Config.Agent.AutoPromote.Enabled {
		setDoctorCurrentCheck("Project " + id + " auto-promote")
		checks = append(checks, checkDoctorAutoPromote(ctx, id, workflow.Config, deps, time.Now()))
	}
	if workflow.Config.Tracker.Kind == workflowconfig.TrackerGitHub {
		setDoctorCurrentCheck("Project " + id + " dependency auto-unblock")
		checks = append(checks, checkDoctorDependencyAutoUnblock(ctx, id, workflow.Config, deps))
		setDoctorCurrentCheck("Project " + id + " blocked recovery")
		checks = append(checks, checkDoctorBlockedRecovery(ctx, id, workflow.Config, deps))
	}

	sourceRepoCheckName := "Project " + id + " source repo"
	setDoctorCurrentCheck(sourceRepoCheckName)
	sourceRoot := projectSourceRoot(project, workflow.Config)
	if sourceRoot == "" {
		return append(checks, doctorCheck{
			Name:   sourceRepoCheckName,
			Status: doctorFail,
			Detail: "source root is not configured",
			Hint:   "Set workspace.source_root, project workdir, or workspace.root to an existing git checkout.",
		})
	}
	expandedSourceRoot, err := expandDoctorWorkspacePath(sourceRoot)
	if err != nil {
		return append(checks, doctorCheck{
			Name:   sourceRepoCheckName,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", sourceRoot, err),
			Hint:   "Set workspace.source_root or project workdir to an existing git checkout.",
		})
	}
	if err := deps.gitWorkTree(ctx, expandedSourceRoot); err != nil {
		return append(checks, doctorCheck{
			Name:   sourceRepoCheckName,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", expandedSourceRoot, err),
			Hint:   "Set workspace.source_root or project workdir to an existing git checkout.",
		})
	}
	checks = append(checks, doctorCheck{
		Name:   sourceRepoCheckName,
		Status: doctorOK,
		Detail: expandedSourceRoot + " is a git worktree",
	})
	if workflow.Config.Tracker.Kind == workflowconfig.TrackerGitHub {
		setDoctorCurrentCheck("Project " + id + " GitHub readiness")
		checks = append(checks, checkDoctorGitHubReadiness(ctx, id, project, workflow.Config, deps, githubToken, expandedSourceRoot)...)
	}
	return checks
}

func doctorProjectID(project globalconfig.Project) string {
	id := strings.TrimSpace(project.ID)
	if id == "" {
		return "project"
	}
	return id
}

func checkDoctorAutoPromote(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps, now time.Time) doctorCheck {
	name := "Project " + id + " auto-promote"
	if !cfg.Agent.AutoPromote.Enabled {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: "agent.auto_promote.enabled=false; live candidate diagnostics disabled",
		}
	}
	if !doctorStateInList("Merging", cfg.Tracker.ActiveStates) {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "agent.auto_promote.enabled=true but tracker.active_states does not include Merging",
			Hint:   "Add Merging to tracker.active_states so promoted issues can enter the merge lane.",
		}
	}
	if cfg.Tracker.Kind != workflowconfig.TrackerGitHub {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: "agent.auto_promote.enabled=true; live GitHub diagnostics skipped for " + cfg.Tracker.Kind + " tracker",
		}
	}

	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("create auto-promote diagnostic connector: %v", err),
			Hint:   "Fix GitHub tracker credentials and ProjectV2 configuration.",
		}
	}
	if projectConnector == nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "create auto-promote diagnostic connector: connector is nil",
			Hint:   "Fix GitHub tracker configuration.",
		}
	}

	check := checkDoctorAutoPromoteLive(ctx, name, cfg, projectConnector, now)
	if err := closeDoctorAutoPromoteConnector(projectConnector); err != nil && check.Status != doctorFail {
		check.Status = doctorWarn
		check.Detail = check.Detail + "; connector close failed: " + err.Error()
		check.Hint = "Rerun detent doctor and check local network resources."
	}
	return check
}

func checkDoctorAutoPromoteLive(
	ctx context.Context,
	name string,
	cfg workflowconfig.Config,
	projectConnector doctorAutoPromoteConnector,
	now time.Time,
) doctorCheck {
	if verifier, ok := projectConnector.(doctorStatusOptionVerifier); ok {
		if err := verifier.VerifyStatusOptions(ctx, []string{"Human Review", "Merging"}); err != nil {
			return doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("status option verification failed: %v", err),
				Hint:   "Ensure Human Review and Merging resolve through tracker.state_map to existing GitHub Project Status options.",
			}
		}
	}

	issues, err := fetchDoctorAutoPromoteIssues(ctx, projectConnector, []string{"Human Review"})
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("fetch Human Review candidates: %v", err),
			Hint:   "Check GitHub Project access, Status field options, and repository pull request access.",
		}
	}

	autoPromoteCfg := doctorAutoPromoteConfig(cfg)
	reasonCounts := map[string]int{}
	candidates := make([]doctorAutoPromoteCandidateDiagnostic, 0, len(issues))
	var quietRemaining time.Duration
	for _, issue := range issues {
		summary := orchestrator.AutoPromoteSummaryFromIssue(issue)
		decision := orchestrator.EvaluateAutoPromote(issue, summary, autoPromoteCfg, now)
		candidate := doctorAutoPromoteCandidateDiagnosticFromIssue(issue, decision)
		candidates = append(candidates, candidate)
		reasonCounts[candidate.Reason]++
		if decision.QuietRemaining > quietRemaining {
			quietRemaining = decision.QuietRemaining
		}
		if decision.Reason == orchestrator.AutoPromoteReasonMissingPullRequest {
			if prNumber, ok := doctorLinkedPullRequestNumber(issue); ok {
				return doctorCheck{
					Name:                  name,
					Status:                doctorFail,
					Detail:                fmt.Sprintf("%s has linked PR #%d but auto-promote readiness reports missing_pull_request", doctorIssueLabel(issue), prNumber),
					Hint:                  "Verify GitHub PR attachment, branch prefix matching, and repository access for Human Review candidates.",
					AutoPromoteCandidates: []doctorAutoPromoteCandidateDiagnostic{candidate},
				}
			}
		}
	}

	detail := fmt.Sprintf(
		"agent.auto_promote.enabled=true; status options resolved; sampled %d Human Review candidate(s)",
		len(issues),
	)
	if len(reasonCounts) > 0 {
		detail += "; reasons: " + doctorAutoPromoteReasonCounts(reasonCounts)
	}
	if quietRemaining > 0 {
		detail += "; max_quiet_remaining=" + quietRemaining.Truncate(time.Second).String()
	}
	if len(candidates) > 0 {
		detail += "; candidates: " + doctorAutoPromoteCandidateSummaries(candidates)
	}
	return doctorCheck{
		Name:                  name,
		Status:                doctorOK,
		Detail:                detail,
		AutoPromoteCandidates: candidates,
	}
}

func fetchDoctorAutoPromoteIssues(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	states []string,
) ([]connector.Issue, error) {
	if limited, ok := projectConnector.(doctorAutoPromoteLimitedConnector); ok {
		return limited.FetchIssuesByStatesLimit(ctx, states, doctorAutoPromoteSampleLimit)
	}
	issues, err := projectConnector.FetchIssuesByStates(ctx, states)
	if err != nil {
		return nil, err
	}
	if len(issues) > doctorAutoPromoteSampleLimit {
		issues = issues[:doctorAutoPromoteSampleLimit]
	}
	return issues, nil
}

func doctorAutoPromoteConfig(cfg workflowconfig.Config) orchestrator.AutoPromoteConfig {
	return orchestrator.AutoPromoteConfig{
		Enabled:            cfg.Agent.AutoPromote.Enabled,
		QuietDuration:      time.Duration(cfg.Agent.AutoPromote.QuietSeconds) * time.Second,
		OptoutLabel:        cfg.Agent.AutoPromote.OptoutLabel,
		AllowedIssueLabels: append([]string(nil), cfg.Agent.AutoPromote.AllowedIssueLabels...),
		Gate:               cfg.Gate,
	}
}

func doctorAutoPromoteCandidateDiagnosticFromIssue(
	issue connector.Issue,
	decision orchestrator.AutoPromoteDecision,
) doctorAutoPromoteCandidateDiagnostic {
	diagnostic := doctorAutoPromoteCandidateDiagnostic{
		IssueID:         strings.TrimSpace(issue.ID),
		IssueIdentifier: strings.TrimSpace(issue.Identifier),
		IssueURL:        strings.TrimSpace(issue.URL),
		Reason:          doctorAutoPromoteDiagnosticReason(issue, decision),
	}
	if decision.QuietRemaining > 0 {
		diagnostic.QuietRemainingSeconds = int64(decision.QuietRemaining.Truncate(time.Second) / time.Second)
	}
	if prNumber, ok := doctorLinkedPullRequestNumber(issue); ok {
		diagnostic.PRNumber = prNumber
	}
	if issue.PullRequest == nil {
		return diagnostic
	}

	pullRequest := issue.PullRequest
	if pullRequest.Number > 0 {
		diagnostic.PRNumber = pullRequest.Number
	}
	diagnostic.PRURL = strings.TrimSpace(pullRequest.URL)
	diagnostic.PRHeadSHA = strings.TrimSpace(pullRequest.HeadSHA)
	diagnostic.PRMergeableState = strings.TrimSpace(pullRequest.MergeableState)
	diagnostic.LatestCodexReviewState = doctorLatestCodexReviewState(pullRequest)
	diagnostic.LatestCodexReviewCommitSHA = strings.TrimSpace(pullRequest.LatestCodexReviewCommitSHA)
	diagnostic.LatestCodexReviewSubmittedAt = doctorLatestCodexReviewSubmittedAt(pullRequest)
	return diagnostic
}

func doctorAutoPromoteDiagnosticReason(issue connector.Issue, decision orchestrator.AutoPromoteDecision) string {
	if decision.Reason == orchestrator.AutoPromoteReasonCodexReviewMissing && doctorPullRequestHasStaleCodexReview(issue.PullRequest) {
		return "stale_automated_review"
	}
	if decision.Reason == orchestrator.AutoPromoteReasonCodexReviewNotQuiet {
		return "quiet_period_remaining"
	}
	return string(decision.Reason)
}

func doctorPullRequestHasStaleCodexReview(pullRequest *connector.PullRequest) bool {
	if pullRequest == nil {
		return false
	}
	headSHA := strings.TrimSpace(pullRequest.HeadSHA)
	reviewCommitSHA := strings.TrimSpace(pullRequest.LatestCodexReviewCommitSHA)
	if headSHA == "" || reviewCommitSHA == "" {
		return false
	}
	if doctorLatestCodexReviewState(pullRequest) == "" && pullRequest.LatestCodexReviewSubmittedAt == nil {
		return false
	}
	return !strings.EqualFold(headSHA, reviewCommitSHA)
}

func doctorLatestCodexReviewState(pullRequest *connector.PullRequest) string {
	if pullRequest == nil {
		return ""
	}
	if state := strings.TrimSpace(pullRequest.LatestCodexReviewState); state != "" {
		return state
	}
	return strings.TrimSpace(pullRequest.CodexReviewState)
}

func doctorLatestCodexReviewSubmittedAt(pullRequest *connector.PullRequest) *time.Time {
	if pullRequest == nil {
		return nil
	}
	if pullRequest.LatestCodexReviewSubmittedAt != nil {
		return pullRequest.LatestCodexReviewSubmittedAt
	}
	return pullRequest.CodexReviewSubmittedAt
}

func doctorAutoPromoteCandidateSummaries(candidates []doctorAutoPromoteCandidateDiagnostic) string {
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		parts = append(parts, doctorAutoPromoteCandidateSummary(candidate))
	}
	return strings.Join(parts, "; ")
}

func doctorAutoPromoteCandidateSummary(candidate doctorAutoPromoteCandidateDiagnostic) string {
	parts := []string{doctorAutoPromoteCandidateIssueLabel(candidate)}
	if candidate.PRNumber > 0 {
		parts = append(parts, fmt.Sprintf("PR #%d", candidate.PRNumber))
	}
	if candidate.PRURL != "" {
		parts = append(parts, candidate.PRURL)
	}
	if candidate.PRHeadSHA != "" {
		parts = append(parts, "head="+candidate.PRHeadSHA)
	}
	if candidate.PRMergeableState != "" {
		parts = append(parts, "mergeable="+candidate.PRMergeableState)
	}
	if candidate.LatestCodexReviewState != "" || candidate.LatestCodexReviewCommitSHA != "" {
		review := strings.TrimSpace(candidate.LatestCodexReviewState)
		if candidate.LatestCodexReviewCommitSHA != "" {
			if review == "" {
				review = "review"
			}
			review += "@" + candidate.LatestCodexReviewCommitSHA
		}
		parts = append(parts, "review="+review)
	}
	if candidate.LatestCodexReviewSubmittedAt != nil {
		parts = append(parts, "submitted="+candidate.LatestCodexReviewSubmittedAt.UTC().Format(time.RFC3339))
	}
	if candidate.QuietRemainingSeconds > 0 {
		parts = append(parts, "quiet_remaining="+(time.Duration(candidate.QuietRemainingSeconds)*time.Second).String())
	}
	parts = append(parts, "reason="+candidate.Reason)
	return strings.Join(parts, " ")
}

func doctorAutoPromoteCandidateIssueLabel(candidate doctorAutoPromoteCandidateDiagnostic) string {
	switch {
	case candidate.IssueID != "" && candidate.IssueIdentifier != "":
		return candidate.IssueID + " (" + candidate.IssueIdentifier + ")"
	case candidate.IssueID != "":
		return candidate.IssueID
	case candidate.IssueIdentifier != "":
		return candidate.IssueIdentifier
	default:
		return "sampled issue"
	}
}

func doctorAutoPromoteReasonCounts(counts map[string]int) string {
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		if strings.TrimSpace(reason) != "" {
			reasons = append(reasons, reason)
		}
	}
	sort.Strings(reasons)

	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s=%d", reason, counts[reason]))
	}
	return strings.Join(parts, ", ")
}

type doctorDependencyAutoUnblockSettings struct {
	Enabled      bool
	SourceStates []string
	TargetState  string
	Readiness    string
}

type doctorDependencyBlocker struct {
	Ref      connector.BlockedRef
	Issue    connector.Issue
	Resolved bool
}

type doctorDependencyDiagnostic struct {
	Code       string
	Issue      connector.Issue
	References []string
}

func checkDoctorBlockedRecovery(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + id + " blocked recovery"
	if cfg.Tracker.Kind != workflowconfig.TrackerGitHub {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: "live blocked recovery diagnostics skipped for " + cfg.Tracker.Kind + " tracker",
		}
	}

	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("create blocked recovery diagnostic connector: %v", err),
			Hint:   "Fix GitHub tracker credentials and ProjectV2 configuration.",
		}
	}
	if projectConnector == nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "create blocked recovery diagnostic connector: connector is nil",
			Hint:   "Fix GitHub tracker configuration.",
		}
	}

	check := checkDoctorBlockedRecoveryLive(ctx, name, projectConnector)
	if err := closeDoctorAutoPromoteConnector(projectConnector); err != nil && check.Status != doctorFail {
		check.Status = doctorWarn
		check.Detail = check.Detail + "; connector close failed: " + err.Error()
		check.Hint = "Rerun detent doctor and check local network resources."
	}
	return check
}

func checkDoctorBlockedRecoveryLive(
	ctx context.Context,
	name string,
	projectConnector doctorAutoPromoteConnector,
) doctorCheck {
	if verifier, ok := projectConnector.(doctorStatusOptionVerifier); ok {
		if err := verifier.VerifyStatusOptions(ctx, []string{"Blocked", "Rework"}); err != nil {
			return doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("status option verification failed: %v", err),
				Hint:   "Ensure Blocked and Rework resolve through tracker.state_map to existing GitHub Project Status options.",
			}
		}
	}

	issues, err := fetchDoctorBlockedRecoveryIssues(ctx, projectConnector)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("fetch blocked candidates: %v", err),
			Hint:   "Check GitHub Project access, Status field options, and repository pull request access.",
		}
	}

	candidates := []doctorBlockedRecoveryCandidateDiagnostic{}
	for _, issue := range issues {
		decision := orchestrator.EvaluateBlockedRecovery(issue)
		if decision.Action != orchestrator.BlockedRecoveryActionRework {
			continue
		}
		candidates = append(candidates, doctorBlockedRecoveryCandidateDiagnosticFromIssue(issue, decision))
	}

	detail := fmt.Sprintf("sampled %d Blocked candidate(s)", len(issues))
	if len(candidates) == 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: detail + "; no agent-recoverable blocked candidates found",
		}
	}
	detail += "; " + doctorBlockedRecoveryCandidateSummaries(candidates)
	return doctorCheck{
		Name:                      name,
		Status:                    doctorWarn,
		Detail:                    detail,
		Hint:                      "Detent can recover these Blocked issues to Rework because the next action is PR maintenance.",
		BlockedRecoveryCandidates: candidates,
	}
}

func fetchDoctorBlockedRecoveryIssues(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
) ([]connector.Issue, error) {
	if limited, ok := projectConnector.(doctorAutoPromoteLimitedConnector); ok {
		return limited.FetchIssuesByStatesLimit(ctx, []string{"Blocked"}, doctorBlockedRecoverySampleLimit)
	}
	issues, err := projectConnector.FetchIssuesByStates(ctx, []string{"Blocked"})
	if err != nil {
		return nil, err
	}
	if len(issues) > doctorBlockedRecoverySampleLimit {
		issues = issues[:doctorBlockedRecoverySampleLimit]
	}
	return issues, nil
}

func doctorBlockedRecoveryCandidateDiagnosticFromIssue(
	issue connector.Issue,
	decision orchestrator.BlockedRecoveryDecision,
) doctorBlockedRecoveryCandidateDiagnostic {
	diagnostic := doctorBlockedRecoveryCandidateDiagnostic{
		IssueID:         strings.TrimSpace(issue.ID),
		IssueIdentifier: strings.TrimSpace(issue.Identifier),
		IssueURL:        strings.TrimSpace(issue.URL),
		TargetState:     strings.TrimSpace(decision.TargetState),
		Reason:          string(decision.Reason),
		Detail:          strings.TrimSpace(decision.Detail),
	}
	if issue.PullRequest == nil {
		return diagnostic
	}
	pullRequest := issue.PullRequest
	diagnostic.PRNumber = pullRequest.Number
	diagnostic.PRURL = strings.TrimSpace(pullRequest.URL)
	diagnostic.PRHeadSHA = strings.TrimSpace(pullRequest.HeadSHA)
	return diagnostic
}

func doctorBlockedRecoveryCandidateSummaries(candidates []doctorBlockedRecoveryCandidateDiagnostic) string {
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		summary := "pr_recoverable_blocked: " + doctorBlockedRecoveryIssueLabel(candidate)
		if candidate.PRNumber > 0 {
			summary += fmt.Sprintf(" PR #%d", candidate.PRNumber)
		}
		if candidate.Reason != "" {
			summary += " reason=" + candidate.Reason
		}
		if candidate.TargetState != "" {
			summary += " target=" + candidate.TargetState
		}
		parts = append(parts, summary)
	}
	return strings.Join(parts, "; ")
}

func doctorBlockedRecoveryIssueLabel(candidate doctorBlockedRecoveryCandidateDiagnostic) string {
	id := strings.TrimSpace(candidate.IssueID)
	identifier := strings.TrimSpace(candidate.IssueIdentifier)
	switch {
	case id != "" && identifier != "":
		return id + " (" + identifier + ")"
	case id != "":
		return id
	case identifier != "":
		return identifier
	default:
		return "sampled issue"
	}
}

func checkDoctorDependencyAutoUnblock(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + id + " dependency auto-unblock"
	if cfg.Tracker.Kind != workflowconfig.TrackerGitHub {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: "live dependency auto-unblock diagnostics skipped for " + cfg.Tracker.Kind + " tracker",
		}
	}
	if cfg.Tracker.DependencyAutoUnblock.Enabled && !doctorStateInList("Rework", cfg.Tracker.ActiveStates) {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "tracker.dependency_auto_unblock.enabled=true but tracker.active_states does not include Rework",
			Hint:   "Add Rework to tracker.active_states so started dependency-unblocked issues can resume.",
		}
	}

	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("create dependency auto-unblock diagnostic connector: %v", err),
			Hint:   "Fix GitHub tracker credentials and ProjectV2 configuration.",
		}
	}
	if projectConnector == nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "create dependency auto-unblock diagnostic connector: connector is nil",
			Hint:   "Fix GitHub tracker configuration.",
		}
	}

	check := checkDoctorDependencyAutoUnblockLive(ctx, name, cfg, projectConnector)
	if err := closeDoctorAutoPromoteConnector(projectConnector); err != nil && check.Status != doctorFail {
		check.Status = doctorWarn
		check.Detail = check.Detail + "; connector close failed: " + err.Error()
		check.Hint = "Rerun detent doctor and check local network resources."
	}
	return check
}

func checkDoctorDependencyAutoUnblockLive(
	ctx context.Context,
	name string,
	cfg workflowconfig.Config,
	projectConnector doctorAutoPromoteConnector,
) doctorCheck {
	dependencyCfg := doctorDependencyAutoUnblockConfig(cfg)
	if verifier, ok := projectConnector.(doctorStatusOptionVerifier); ok {
		states := append([]string(nil), dependencyCfg.SourceStates...)
		if dependencyCfg.Enabled {
			states = append(states, dependencyCfg.TargetState)
			states = append(states, "Rework")
		}
		if len(states) > 0 {
			if err := verifier.VerifyStatusOptions(ctx, states); err != nil {
				return doctorCheck{
					Name:   name,
					Status: doctorFail,
					Detail: fmt.Sprintf("status option verification failed: %v", err),
					Hint:   "Ensure dependency auto-unblock source_states, target_state, and Rework resolve through tracker.state_map to existing GitHub Project Status options.",
				}
			}
		}
	}

	issues, err := fetchDoctorDependencyAutoUnblockIssues(ctx, projectConnector, dependencyCfg.SourceStates)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("fetch dependency waiting candidates: %v", err),
			Hint:   "Check GitHub Project access, Status field options, and repository issue access.",
		}
	}

	diagnostics, err := doctorDependencyDiagnostics(ctx, projectConnector, dependencyCfg, cfg.Tracker.TerminalStates, issues)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("inspect dependency waiting candidates: %v", err),
			Hint:   "Check GitHub issue access and dependency references.",
		}
	}

	detail := doctorDependencyAutoUnblockDetail(dependencyCfg, len(issues), diagnostics)
	if len(diagnostics) == 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: detail,
		}
	}
	return doctorCheck{
		Name:   name,
		Status: doctorWarn,
		Detail: detail,
		Hint:   doctorDependencyAutoUnblockHint(diagnostics),
	}
}

func doctorDependencyAutoUnblockConfig(cfg workflowconfig.Config) doctorDependencyAutoUnblockSettings {
	dependencyCfg := cfg.Tracker.DependencyAutoUnblock
	dependencyCfg.Normalize()
	sourceStates := doctorDependencySourceStates(dependencyCfg.SourceStates)
	targetState := strings.TrimSpace(dependencyCfg.TargetState)
	if targetState == "" {
		targetState = "Todo"
	}
	readiness := strings.ToLower(strings.TrimSpace(dependencyCfg.Readiness))
	if readiness == "" {
		readiness = workflowconfig.DependencyReadinessTerminalOrMerged
	}
	return doctorDependencyAutoUnblockSettings{
		Enabled:      dependencyCfg.Enabled,
		SourceStates: sourceStates,
		TargetState:  targetState,
		Readiness:    readiness,
	}
}

func doctorDependencySourceStates(states []string) []string {
	if len(states) == 0 {
		states = []string{"Blocked"}
	}
	out := make([]string, 0, len(states))
	seen := make(map[string]struct{}, len(states))
	for _, state := range states {
		display := doctorDisplayStateName(state)
		key := strings.ToLower(strings.TrimSpace(display))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, display)
	}
	return out
}

func doctorDisplayStateName(state string) string {
	state = strings.TrimSpace(state)
	switch strings.ToLower(state) {
	case "blocked":
		return "Blocked"
	case "human review":
		return "Human Review"
	case "merging":
		return "Merging"
	case "rework":
		return "Rework"
	case "todo":
		return "Todo"
	case "in progress":
		return "In Progress"
	default:
		return state
	}
}

func fetchDoctorDependencyAutoUnblockIssues(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	states []string,
) ([]connector.Issue, error) {
	if limited, ok := projectConnector.(doctorAutoPromoteLimitedConnector); ok {
		return limited.FetchIssuesByStatesLimit(ctx, states, doctorDependencyAutoUnblockSampleLimit)
	}
	issues, err := projectConnector.FetchIssuesByStates(ctx, states)
	if err != nil {
		return nil, err
	}
	if len(issues) > doctorDependencyAutoUnblockSampleLimit {
		issues = issues[:doctorDependencyAutoUnblockSampleLimit]
	}
	return issues, nil
}

func doctorDependencyDiagnostics(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	cfg doctorDependencyAutoUnblockSettings,
	terminalStates []string,
	issues []connector.Issue,
) ([]doctorDependencyDiagnostic, error) {
	diagnostics := []doctorDependencyDiagnostic{}
	for _, issue := range issues {
		hydrated, ok, err := hydrateDoctorDependencyIssue(ctx, projectConnector, issue, cfg.SourceStates)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		references := doctorDependencyReferenceLabels(hydrated)
		if len(references) == 0 {
			continue
		}
		if !cfg.Enabled {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_auto_unblock_disabled",
				Issue:      hydrated,
				References: references,
			})
			continue
		}
		if len(hydrated.BlockedBy) > 0 && len(doctorDependencyTextBlockedRefs(hydrated)) == 0 {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_metadata_missing",
				Issue:      hydrated,
				References: references,
			})
			continue
		}
		if len(hydrated.BlockedBy) == 0 {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_reference_unresolved",
				Issue:      hydrated,
				References: references,
			})
			continue
		}

		blockers, err := resolveDoctorDependencyBlockers(ctx, projectConnector, hydrated)
		if err != nil {
			return nil, err
		}
		if unresolved := unresolvedDoctorDependencyReferences(blockers); len(unresolved) > 0 {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_reference_unresolved",
				Issue:      hydrated,
				References: unresolved,
			})
			continue
		}
		if doctorDependencyBlockersReady(blockers, cfg, terminalStates) {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_ready_but_still_blocked",
				Issue:      hydrated,
				References: doctorDependencyBlockerLabels(blockers),
			})
		}
	}
	return diagnostics, nil
}

func hydrateDoctorDependencyIssue(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	issue connector.Issue,
	sourceStates []string,
) (connector.Issue, bool, error) {
	issue = doctorIssueWithTextDependencyRefs(issue)
	if strings.TrimSpace(issue.Identifier) == "" {
		return issue, doctorStateInList(issue.State, sourceStates), nil
	}
	resolver, ok := projectConnector.(connector.IssueReferenceResolver)
	if !ok {
		return issue, doctorStateInList(issue.State, sourceStates), nil
	}
	issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, []string{issue.Identifier})
	if err != nil {
		return connector.Issue{}, false, err
	}
	for _, hydrated := range issues {
		if sameDoctorIssueIdentity(issue, hydrated) {
			previousBlockedBy := append([]connector.BlockedRef(nil), issue.BlockedBy...)
			merged := mergeDoctorDependencyIssue(issue, hydrated)
			merged.BlockedBy = mergeDoctorDependencyBlockedRefs(previousBlockedBy, merged.BlockedBy)
			merged = doctorIssueWithTextDependencyRefs(merged)
			return merged, doctorStateInList(merged.State, sourceStates), nil
		}
	}
	return issue, doctorStateInList(issue.State, sourceStates), nil
}

func resolveDoctorDependencyBlockers(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	issue connector.Issue,
) ([]doctorDependencyBlocker, error) {
	blockers := make([]doctorDependencyBlocker, 0, len(issue.BlockedBy))
	identifiers := make([]string, 0, len(issue.BlockedBy))
	seen := map[string]struct{}{}
	for _, ref := range issue.BlockedBy {
		ref.Identifier = strings.TrimSpace(ref.Identifier)
		ref.ID = strings.TrimSpace(ref.ID)
		ref.State = strings.TrimSpace(ref.State)
		blockers = append(blockers, doctorDependencyBlocker{Ref: ref})
		if ref.Identifier == "" {
			continue
		}
		key := strings.ToLower(ref.Identifier)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		identifiers = append(identifiers, ref.Identifier)
	}

	resolver, ok := projectConnector.(connector.IssueReferenceResolver)
	if !ok || len(identifiers) == 0 {
		return blockers, nil
	}
	issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		return nil, err
	}
	byIdentifier := make(map[string]connector.Issue, len(issues))
	for _, blocker := range issues {
		identifier := strings.ToLower(strings.TrimSpace(blocker.Identifier))
		if identifier != "" {
			byIdentifier[identifier] = blocker
		}
	}
	for index := range blockers {
		identifier := strings.ToLower(strings.TrimSpace(blockers[index].Ref.Identifier))
		blocker, ok := byIdentifier[identifier]
		if !ok {
			continue
		}
		blockers[index].Issue = blocker
		blockers[index].Resolved = true
		blockers[index].Ref.ID = doctorFirstNonBlank(blocker.ID, blockers[index].Ref.ID)
		blockers[index].Ref.Identifier = doctorFirstNonBlank(blocker.Identifier, blockers[index].Ref.Identifier)
		blockers[index].Ref.State = doctorFirstNonBlank(blocker.State, blockers[index].Ref.State)
	}
	return blockers, nil
}

func unresolvedDoctorDependencyReferences(blockers []doctorDependencyBlocker) []string {
	refs := []string{}
	for _, blocker := range blockers {
		if blocker.Resolved {
			continue
		}
		ref := doctorDependencyRefLabel(blocker.Ref)
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

func doctorDependencyBlockersReady(
	blockers []doctorDependencyBlocker,
	cfg doctorDependencyAutoUnblockSettings,
	terminalStates []string,
) bool {
	if len(blockers) == 0 {
		return false
	}
	for _, blocker := range blockers {
		if !doctorDependencyBlockerReady(blocker, cfg, terminalStates) {
			return false
		}
	}
	return true
}

func doctorDependencyBlockerReady(
	blocker doctorDependencyBlocker,
	cfg doctorDependencyAutoUnblockSettings,
	terminalStates []string,
) bool {
	if blocker.Resolved {
		if blocker.Issue.Closed || doctorStateInList(blocker.Issue.State, terminalStates) {
			return true
		}
		if cfg.Readiness == workflowconfig.DependencyReadinessTerminalOrMerged && doctorPullRequestMerged(blocker.Issue.PullRequest) {
			return true
		}
		return false
	}
	if strings.TrimSpace(blocker.Ref.State) == "" {
		return false
	}
	return doctorStateInList(blocker.Ref.State, terminalStates)
}

func doctorPullRequestMerged(pullRequest *connector.PullRequest) bool {
	return pullRequest != nil && strings.EqualFold(strings.TrimSpace(pullRequest.State), "merged")
}

func doctorDependencyAutoUnblockDetail(
	cfg doctorDependencyAutoUnblockSettings,
	sampled int,
	diagnostics []doctorDependencyDiagnostic,
) string {
	status := "tracker.dependency_auto_unblock.enabled=false"
	if cfg.Enabled {
		status = "tracker.dependency_auto_unblock.enabled=true"
	}
	detail := fmt.Sprintf(
		"%s; sampled %d dependency waiting candidate(s) from source_states=%s",
		status,
		sampled,
		strings.Join(cfg.SourceStates, ","),
	)
	if len(diagnostics) == 0 {
		return detail + "; no stalled dependency candidates found"
	}
	parts := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		parts = append(parts, doctorDependencyDiagnosticDetail(diagnostic))
	}
	return detail + "; " + strings.Join(parts, "; ")
}

func doctorDependencyDiagnosticDetail(diagnostic doctorDependencyDiagnostic) string {
	return fmt.Sprintf(
		"%s: %s references %s",
		diagnostic.Code,
		doctorIssueLabel(diagnostic.Issue),
		strings.Join(diagnostic.References, ", "),
	)
}

func doctorDependencyAutoUnblockHint(diagnostics []doctorDependencyDiagnostic) string {
	codes := map[string]struct{}{}
	for _, diagnostic := range diagnostics {
		codes[diagnostic.Code] = struct{}{}
	}
	hints := []string{}
	if _, ok := codes["dependency_auto_unblock_disabled"]; ok {
		hints = append(hints, "Set tracker.dependency_auto_unblock.enabled: true and ensure source_states include the waiting Status values.")
	}
	if _, ok := codes["dependency_reference_unresolved"]; ok {
		hints = append(hints, "Fix issue content so Depends on: or Blocked by: references point to existing GitHub issues.")
	}
	if _, ok := codes["dependency_metadata_missing"]; ok {
		hints = append(hints, "Add canonical Depends on: or Blocked by: lines to the blocked issue body before leaving dependency-blocked work in Blocked.")
	}
	if _, ok := codes["dependency_ready_but_still_blocked"]; ok {
		hints = append(hints, "Check tracker.dependency_auto_unblock source_states, target_state, readiness, and GitHub Project Status mappings.")
	}
	return strings.Join(hints, " ")
}

func doctorDependencyReferenceLabels(issue connector.Issue) []string {
	refs := doctorBlockedRefLabels(issue.BlockedBy)
	if len(refs) > 0 {
		return refs
	}
	return doctorDependencyLineReferences(issue.Description)
}

func doctorBlockedRefLabels(refs []connector.BlockedRef) []string {
	labels := make([]string, 0, len(refs))
	seen := map[string]struct{}{}
	for _, ref := range refs {
		label := doctorDependencyRefLabel(ref)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func doctorDependencyLineReferences(body string) []string {
	refs := []string{}
	seen := map[string]struct{}{}
	for _, line := range strings.FieldsFunc(body, func(r rune) bool {
		return r == '\n' || r == '\r'
	}) {
		ref, ok := doctorDependencyLineReference(line)
		if !ok {
			continue
		}
		key := strings.ToLower(ref)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

func doctorIssueWithTextDependencyRefs(issue connector.Issue) connector.Issue {
	issue.BlockedBy = mergeDoctorDependencyBlockedRefs(issue.BlockedBy, doctorDependencyTextBlockedRefs(issue))
	issue.BlockedBy = doctorDependencyBlockedRefsWithoutSelf(issue.BlockedBy, issue.Identifier)
	return issue
}

func doctorDependencyTextBlockedRefs(issue connector.Issue) []connector.BlockedRef {
	repo := doctorDependencyIssueRepo(issue.Identifier)
	refs := []connector.BlockedRef{}
	seen := map[string]struct{}{}
	appendIdentifier := func(identifier string) {
		identifier = strings.TrimSpace(identifier)
		if identifier == "" {
			return
		}
		key := strings.ToLower(identifier)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		refs = append(refs, connector.BlockedRef{Identifier: identifier})
	}
	for _, lineRef := range doctorDependencyLineReferences(issue.Description) {
		identifiers := doctorDependencyIssueIdentifiersInText(lineRef, repo)
		if len(identifiers) == 0 {
			appendIdentifier(lineRef)
			continue
		}
		for _, identifier := range identifiers {
			appendIdentifier(identifier)
		}
	}
	return refs
}

func mergeDoctorDependencyBlockedRefs(existing []connector.BlockedRef, incoming []connector.BlockedRef) []connector.BlockedRef {
	if len(existing) == 0 && len(incoming) == 0 {
		return nil
	}
	merged := make([]connector.BlockedRef, 0, len(existing)+len(incoming))
	seen := map[string]struct{}{}
	appendRefs := func(refs []connector.BlockedRef) {
		for _, ref := range refs {
			key := strings.ToLower(strings.TrimSpace(ref.Identifier))
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(ref.ID))
			}
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, ref)
		}
	}
	appendRefs(existing)
	appendRefs(incoming)
	return merged
}

func doctorDependencyBlockedRefsWithoutSelf(refs []connector.BlockedRef, identifier string) []connector.BlockedRef {
	self := strings.ToLower(strings.TrimSpace(identifier))
	if self == "" || len(refs) == 0 {
		return refs
	}
	filtered := refs[:0]
	for _, ref := range refs {
		if strings.ToLower(strings.TrimSpace(ref.Identifier)) == self {
			continue
		}
		filtered = append(filtered, ref)
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func doctorDependencyIssueIdentifiersInText(text string, repo string) []string {
	refs := []string{}
	seen := map[string]struct{}{}
	appendIdentifier := func(refRepo string, number string) {
		identifier := doctorDependencyBlockerIdentifier(refRepo, number, repo)
		if identifier == "" {
			return
		}
		key := strings.ToLower(identifier)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		refs = append(refs, identifier)
	}
	for _, matches := range doctorDependencyIssueURLPattern.FindAllStringSubmatch(text, -1) {
		if len(matches) == 3 {
			appendIdentifier(matches[1], matches[2])
		}
	}
	for _, matches := range doctorDependencyIssueRefPattern.FindAllStringSubmatch(text, -1) {
		if len(matches) == 3 {
			appendIdentifier(matches[1], matches[2])
		}
	}
	return refs
}

func doctorDependencyIssueRepo(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	index := strings.LastIndex(identifier, "#")
	if index <= 0 {
		return ""
	}
	return strings.TrimSpace(identifier[:index])
}

func doctorDependencyBlockerIdentifier(refRepo string, number string, repo string) string {
	if strings.TrimSpace(number) == "" {
		return ""
	}
	refRepo = strings.TrimSpace(refRepo)
	if refRepo == "" {
		if repo == "" {
			return "#" + strings.TrimSpace(number)
		}
		refRepo = repo
	}
	return refRepo + "#" + strings.TrimSpace(number)
}

func doctorDependencyLineReference(line string) (string, bool) {
	line = strings.TrimSpace(line)
	for {
		switch {
		case strings.HasPrefix(line, ">"):
			line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
		case strings.HasPrefix(line, "- "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
		case strings.HasPrefix(line, "* "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "* "))
		case strings.HasPrefix(line, "+ "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "+ "))
		default:
			goto trimmed
		}
	}
trimmed:
	lower := strings.ToLower(line)
	for _, prefix := range []string{"depends on:", "depends-on:", "blocked by:"} {
		if strings.HasPrefix(lower, prefix) {
			ref := strings.TrimSpace(line[len(prefix):])
			return ref, ref != ""
		}
	}
	return "", false
}

func doctorDependencyBlockerLabels(blockers []doctorDependencyBlocker) []string {
	labels := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		if blocker.Resolved {
			if identifier := strings.TrimSpace(blocker.Issue.Identifier); identifier != "" {
				labels = append(labels, identifier)
				continue
			}
			if id := strings.TrimSpace(blocker.Issue.ID); id != "" {
				labels = append(labels, id)
				continue
			}
		}
		if label := doctorDependencyRefLabel(blocker.Ref); label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

func doctorDependencyRefLabel(ref connector.BlockedRef) string {
	if identifier := strings.TrimSpace(ref.Identifier); identifier != "" {
		return identifier
	}
	return strings.TrimSpace(ref.ID)
}

func sameDoctorIssueIdentity(left connector.Issue, right connector.Issue) bool {
	leftID := strings.TrimSpace(left.ID)
	rightID := strings.TrimSpace(right.ID)
	if leftID != "" && rightID != "" && leftID == rightID {
		return true
	}
	leftIdentifier := strings.ToLower(strings.TrimSpace(left.Identifier))
	rightIdentifier := strings.ToLower(strings.TrimSpace(right.Identifier))
	return leftIdentifier != "" && leftIdentifier == rightIdentifier
}

func mergeDoctorDependencyIssue(left connector.Issue, right connector.Issue) connector.Issue {
	merged := left
	if strings.TrimSpace(right.ID) != "" {
		merged.ID = right.ID
	}
	if strings.TrimSpace(right.Identifier) != "" {
		merged.Identifier = right.Identifier
	}
	if strings.TrimSpace(right.Title) != "" {
		merged.Title = right.Title
	}
	if strings.TrimSpace(right.Description) != "" {
		merged.Description = right.Description
	}
	if strings.TrimSpace(right.State) != "" {
		merged.State = right.State
	}
	if strings.TrimSpace(right.URL) != "" {
		merged.URL = right.URL
	}
	if len(right.BlockedBy) > 0 {
		merged.BlockedBy = right.BlockedBy
	}
	if strings.TrimSpace(right.BlockerReason) != "" {
		merged.BlockerReason = right.BlockerReason
	}
	return merged
}

func doctorFirstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func doctorLinkedPullRequestNumber(issue connector.Issue) (int, bool) {
	if issue.PRNumber == nil || *issue.PRNumber <= 0 {
		return 0, false
	}
	return *issue.PRNumber, true
}

func doctorIssueLabel(issue connector.Issue) string {
	id := strings.TrimSpace(issue.ID)
	identifier := strings.TrimSpace(issue.Identifier)
	switch {
	case id != "" && identifier != "":
		return id + " (" + identifier + ")"
	case id != "":
		return id
	case identifier != "":
		return identifier
	default:
		return "sampled issue"
	}
}

func doctorStateInList(state string, states []string) bool {
	state = strings.ToLower(strings.TrimSpace(state))
	if state == "" {
		return false
	}
	for _, candidate := range states {
		if strings.ToLower(strings.TrimSpace(candidate)) == state {
			return true
		}
	}
	return false
}

func closeDoctorAutoPromoteConnector(projectConnector doctorAutoPromoteConnector) error {
	closer, ok := projectConnector.(connector.Closer)
	if !ok {
		return nil
	}
	return closer.Close()
}

func defaultDoctorAutoPromoteConnector(cfg workflowconfig.Config) (doctorAutoPromoteConnector, error) {
	return factory.NewFromConfig(factory.Config{
		Kind:                    cfg.Tracker.Kind,
		Memory:                  memory.Config{Issues: cfg.Tracker.Issues},
		Endpoint:                cfg.Tracker.Endpoint,
		APIKey:                  cfg.Tracker.APIKey,
		HTTPMaxIdleConns:        cfg.Tracker.HTTPMaxIdleConns,
		HTTPMaxIdleConnsPerHost: cfg.Tracker.HTTPMaxIdleConnsPerHost,
		HTTPIdleConnTimeoutMS:   cfg.Tracker.HTTPIdleConnTimeoutMS,
		GitHubAppID:             cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:     cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath: cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID: cfg.Tracker.GitHubAppInstallationID,
		GitHubStatusSource:      cfg.Tracker.GitHubStatusSource,
		ProjectSlug:             cfg.Tracker.ProjectSlug,
		Repository:              cfg.Tracker.Repository,
		StatusField:             cfg.Tracker.StatusField,
		StatusLabelPrefix:       cfg.Tracker.StatusLabelPrefix,
		ActiveStates:            cfg.Tracker.ActiveStates,
		ObservedStates:          cfg.Tracker.ObservedStates,
		TerminalStates:          cfg.Tracker.TerminalStates,
		StateMap:                doctorTrackerStateMap(cfg.Tracker.StateMap),
	})
}

func checkDoctorGitHubReadiness(
	ctx context.Context,
	id string,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	deps doctorDeps,
	githubToken RuntimeSecret,
	sourceRoot string,
) []doctorCheck {
	checks, err := deps.githubReadiness(ctx, doctorGitHubConnectorConfig(cfg), doctorGitHubReadinessConfig(ctx, project, cfg, deps, githubToken, sourceRoot))
	if err != nil {
		return []doctorCheck{{
			Name:   "Project " + id + " GitHub readiness",
			Status: doctorFail,
			Detail: "create GitHub readiness checker: " + err.Error(),
			Hint:   "Fix GitHub tracker configuration and credentials.",
		}}
	}
	out := make([]doctorCheck, 0, len(checks))
	for _, check := range checks {
		out = append(out, doctorCheck{
			Name:   "Project " + id + " " + check.Name,
			Status: doctorStatusFromGitHubReadiness(check.Status),
			Detail: check.Detail,
			Hint:   check.Hint,
		})
	}
	return out
}

func doctorGitHubConnectorConfig(cfg workflowconfig.Config) ghconnector.Config {
	return ghconnector.Config{
		Endpoint: cfg.Tracker.Endpoint,
		APIKey:   cfg.Tracker.APIKey,
		HTTPTransport: ghconnector.HTTPTransportConfig{
			MaxIdleConns:        cfg.Tracker.HTTPMaxIdleConns,
			MaxIdleConnsPerHost: cfg.Tracker.HTTPMaxIdleConnsPerHost,
			IdleConnTimeout:     time.Duration(cfg.Tracker.HTTPIdleConnTimeoutMS) * time.Millisecond,
		},
		GitHubAppID:             cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:     cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath: cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID: cfg.Tracker.GitHubAppInstallationID,
		GitHubStatusSource:      cfg.Tracker.GitHubStatusSource,
		ProjectSlug:             cfg.Tracker.ProjectSlug,
		Repository:              cfg.Tracker.Repository,
		StatusField:             cfg.Tracker.StatusField,
		StatusLabelPrefix:       cfg.Tracker.StatusLabelPrefix,
		ActiveStates:            cfg.Tracker.ActiveStates,
		ObservedStates:          cfg.Tracker.ObservedStates,
		TerminalStates:          cfg.Tracker.TerminalStates,
		StateMap:                doctorTrackerStateMap(cfg.Tracker.StateMap),
	}
}

func doctorGitHubReadinessConfig(
	ctx context.Context,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	deps doctorDeps,
	githubToken RuntimeSecret,
	sourceRoot string,
) ghconnector.ReadinessConfig {
	return ghconnector.ReadinessConfig{
		AuthPath:                      doctorGitHubAuthPath(cfg, githubToken, deps.lookupEnv),
		WriteProbeIssue:               cfg.Tracker.WriteProbeIssue,
		Repositories:                  doctorGitHubRepositories(ctx, project, cfg, deps, sourceRoot),
		StatusStates:                  doctorRequiredGitHubStatusStates(cfg),
		ReadStates:                    doctorRequiredGitHubReadStates(cfg),
		RequireProjectRead:            doctorRequiresProjectRead(cfg),
		RequireIssueFieldRead:         doctorRequiresIssueFieldRead(cfg),
		RequireIssueCommentsRead:      doctorRequiresIssueCommentsRead(cfg),
		RequireDependencyMetadataRead: doctorRequiresDependencyMetadataRead(cfg),
		RequireIssueChildrenRead:      doctorRequiresIssueChildrenRead(cfg),
		RequireIssueParentsRead:       doctorRequiresIssueParentsRead(cfg),
		RequirePullRequestRead:        doctorRequiresPullRequestRead(cfg),
		RequirePullRequestReviews:     doctorRequiresPullRequestReviewsRead(cfg),
		RequirePullRequestChecks:      doctorRequiresPullRequestChecksRead(cfg),
		RequireProjectStatusWrite:     doctorRequiresProjectStatusWrite(cfg),
		RequireIssueFieldStatusWrite:  doctorRequiresIssueFieldStatusWrite(cfg),
		RequireLabelStatusWrite:       doctorRequiresLabelStatusWrite(cfg),
		RequireIssueComments:          doctorRequiresIssueCommentWrite(cfg),
		RequireAssigneeWrite:          doctorRequiresAssigneeWrite(cfg),
		RequireIssueClose:             doctorRequiresIssueClose(cfg),
		ProjectFieldWrites:            doctorRequiredProjectFieldWrites(cfg),
	}
}

func doctorStatusFromGitHubReadiness(status ghconnector.ReadinessStatus) doctorStatus {
	switch status {
	case ghconnector.ReadinessOK:
		return doctorOK
	case ghconnector.ReadinessWarn:
		return doctorWarn
	case ghconnector.ReadinessFail:
		return doctorFail
	default:
		return doctorWarn
	}
}

func doctorGitHubAuthPath(cfg workflowconfig.Config, token RuntimeSecret, lookupEnv func(string) string) string {
	if trackerHasGitHubAppCredentials(cfg.Tracker, lookupEnv) {
		return "GitHub App installation token"
	}
	if token.ResolvedVia == "gh" {
		return "gh-resolved token"
	}
	switch strings.TrimSpace(token.Source) {
	case "GITHUB_TOKEN":
		return "GITHUB_TOKEN PAT"
	case "github_token":
		return "global github_token PAT"
	case "":
		if strings.TrimSpace(cfg.Tracker.APIKey) != "" {
			return "workflow tracker.api_key"
		}
		return "GitHub token"
	default:
		return token.Source + " PAT"
	}
}

func doctorRequiredGitHubStatusStates(cfg workflowconfig.Config) []string {
	return uniqueDoctorStrings(append(append([]string{}, cfg.Tracker.ActiveStates...), append(cfg.Tracker.ObservedStates, cfg.Tracker.TerminalStates...)...))
}

func doctorRequiredGitHubReadStates(cfg workflowconfig.Config) []string {
	return uniqueDoctorStrings(append(append([]string{}, cfg.Tracker.ActiveStates...), cfg.Tracker.ObservedStates...))
}

func doctorRequiresProjectRead(cfg workflowconfig.Config) bool {
	return cfg.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceProjectV2
}

func doctorRequiresIssueFieldRead(cfg workflowconfig.Config) bool {
	return cfg.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceIssueField
}

func doctorRequiresLabelRead(cfg workflowconfig.Config) bool {
	return cfg.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceLabel
}

func doctorRequiresStatusWrite(cfg workflowconfig.Config) bool {
	return doctorKanbanIntegrationEnabled(cfg) ||
		len(cfg.Tracker.ActiveStates) > 0 ||
		cfg.Agent.AutoPromote.Enabled ||
		cfg.Tracker.DependencyAutoUnblock.Enabled
}

func doctorRequiresProjectStatusWrite(cfg workflowconfig.Config) bool {
	return doctorRequiresProjectRead(cfg) && doctorRequiresStatusWrite(cfg)
}

func doctorRequiresIssueFieldStatusWrite(cfg workflowconfig.Config) bool {
	return doctorRequiresIssueFieldRead(cfg) && doctorRequiresStatusWrite(cfg)
}

func doctorRequiresLabelStatusWrite(cfg workflowconfig.Config) bool {
	return doctorRequiresLabelRead(cfg) && doctorRequiresStatusWrite(cfg)
}

func doctorRequiresIssueCommentWrite(cfg workflowconfig.Config) bool {
	return doctorKanbanIntegrationEnabled(cfg) ||
		cfg.Agent.AutoPromote.Enabled ||
		cfg.Tracker.DependencyAutoUnblock.Enabled ||
		doctorRequiresIssueClose(cfg)
}

func doctorKanbanIntegrationEnabled(cfg workflowconfig.Config) bool {
	kanban := cfg.Server.Kanban
	kanban.Normalize()
	return kanban.Mode == workflowconfig.KanbanModeIntegration
}

func doctorRequiresIssueCommentsRead(cfg workflowconfig.Config) bool {
	return doctorStateInList("Blocked", cfg.Tracker.ObservedStates) ||
		doctorStateInList("Blocked", cfg.Tracker.ActiveStates) ||
		doctorStateInList("Blocked", cfg.Tracker.DependencyAutoUnblock.SourceStates)
}

func doctorRequiresDependencyMetadataRead(cfg workflowconfig.Config) bool {
	return len(cfg.Tracker.ActiveStates) > 0 ||
		cfg.Tracker.DependencyAutoUnblock.Enabled ||
		doctorRequiresIssueParentsRead(cfg)
}

func doctorRequiresIssueChildrenRead(cfg workflowconfig.Config) bool {
	return doctorRequiresIssueClose(cfg)
}

func doctorRequiresIssueParentsRead(cfg workflowconfig.Config) bool {
	return doctorRequiresIssueClose(cfg)
}

func doctorRequiresAssigneeWrite(cfg workflowconfig.Config) bool {
	if !cfg.Tracker.Claims.Enabled {
		return false
	}
	identity := cfg.Identity
	identity.Normalize()
	return identity.OwnershipMode != workflowconfig.IdentityOwnershipField
}

func doctorRequiresIssueClose(cfg workflowconfig.Config) bool {
	return len(cfg.Tracker.TerminalStates) > 0
}

func doctorRequiresPullRequestRead(cfg workflowconfig.Config) bool {
	return len(cfg.Tracker.ActiveStates) > 0 ||
		cfg.Agent.AutoPromote.Enabled ||
		doctorStateInList("Human Review", cfg.Tracker.ObservedStates) ||
		doctorStateInList("Merging", cfg.Tracker.ActiveStates)
}

func doctorRequiresPullRequestReviewsRead(cfg workflowconfig.Config) bool {
	return doctorRequiresPullRequestRead(cfg)
}

func doctorRequiresPullRequestChecksRead(cfg workflowconfig.Config) bool {
	return doctorRequiresPullRequestRead(cfg)
}

func doctorRequiredProjectFieldWrites(cfg workflowconfig.Config) []ghconnector.ReadinessProjectFieldWrite {
	if !doctorRequiresProjectRead(cfg) {
		return nil
	}
	if !cfg.Tracker.Claims.Enabled {
		return nil
	}
	fields := []ghconnector.ReadinessProjectFieldWrite{}
	if field := strings.TrimSpace(cfg.Tracker.Claims.LeaseField); field != "" {
		fields = append(fields, ghconnector.ReadinessProjectFieldWrite{Name: field})
	}
	identity := cfg.Identity
	identity.Normalize()
	if identity.OwnershipMode == workflowconfig.IdentityOwnershipField {
		if field := strings.TrimSpace(identity.OwnerField); field != "" {
			fields = append(fields, ghconnector.ReadinessProjectFieldWrite{Name: field})
		}
	}
	return fields
}

func doctorGitHubRepositories(
	ctx context.Context,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	deps doctorDeps,
	sourceRoot string,
) []string {
	repositories := []string{}
	if strings.TrimSpace(cfg.Tracker.Repository) != "" {
		repositories = append(repositories, cfg.Tracker.Repository)
	}
	if repo, ok := doctorGitHubRepositoryFromProbe(cfg.Tracker.WriteProbeIssue); ok {
		repositories = append(repositories, repo)
	}
	if repo, ok := doctorGitHubRepositoryFromProbe(project.Workdir); ok {
		repositories = append(repositories, repo)
	}
	if strings.TrimSpace(sourceRoot) != "" && deps.gitRemoteURL != nil {
		if remote, err := deps.gitRemoteURL(ctx, sourceRoot); err == nil {
			if repo, ok := doctorGitHubRepositoryFromRemoteURL(remote); ok {
				repositories = append(repositories, repo)
			}
		}
	}
	return uniqueDoctorStrings(repositories)
}

func doctorGitHubRepositoryFromProbe(value string) (string, bool) {
	repo, _, ok := strings.Cut(strings.TrimSpace(value), "#")
	if !ok {
		return "", false
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return "", false
	}
	return strings.TrimSpace(owner) + "/" + strings.TrimSpace(name), true
}

func doctorGitHubRepositoryFromRemoteURL(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", false
	}
	if after, ok := strings.CutPrefix(remote, "git@github.com:"); ok {
		return doctorCleanGitHubRepository(after)
	}
	if parsed, err := url.Parse(remote); err == nil && strings.EqualFold(parsed.Hostname(), "github.com") {
		return doctorCleanGitHubRepository(strings.TrimPrefix(parsed.Path, "/"))
	}
	return "", false
}

func doctorCleanGitHubRepository(path string) (string, bool) {
	path = strings.TrimSpace(strings.TrimSuffix(path, ".git"))
	parts := strings.Split(path, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return strings.TrimSpace(parts[0]) + "/" + strings.TrimSpace(parts[1]), true
}

func uniqueDoctorStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func doctorTrackerStateMap(value workflowconfig.StringOrMap) map[string]string {
	if !value.IsMap {
		return nil
	}

	out := make(map[string]string, len(value.Map))
	for state, mapped := range value.Map {
		mappedState, ok := mapped.(string)
		if !ok {
			continue
		}
		state = strings.TrimSpace(state)
		mappedState = strings.TrimSpace(mappedState)
		if state != "" && mappedState != "" {
			out[state] = mappedState
		}
	}
	return out
}

func doctorWorkflowConfigWithRuntimeGitHubToken(cfg workflowconfig.Config, token string) workflowconfig.Config {
	token = strings.TrimSpace(token)
	if token != "" && cfg.Tracker.Kind == workflowconfig.TrackerGitHub {
		cfg.Tracker.APIKey = token
	}
	return cfg
}

func checkDoctorConfigReload(cfg globalconfig.Config) doctorCheck {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return doctorCheck{
			Name:   "Global config reload",
			Status: doctorWarn,
			Detail: "global config path is unavailable",
			Hint:   "Fix config resolution, then rerun detent doctor.",
		}
	}

	info, err := os.Lstat(path)
	if err != nil {
		return doctorCheck{
			Name:   "Global config reload",
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s: %v", path, err),
			Hint:   "Fix the global config path before relying on live reload.",
		}
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return doctorCheck{
			Name:   "Global config reload",
			Status: doctorOK,
			Detail: path + " is watched for live reload",
		}
	}

	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return doctorCheck{
			Name:   "Global config reload",
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s is a symlink but its target cannot be resolved: %v", path, err),
			Hint:   "Fix the symlink target before relying on live reload.",
		}
	}
	return doctorCheck{
		Name:   "Global config reload",
		Status: doctorOK,
		Detail: fmt.Sprintf("%s is a symlink to %s; live reload watches the configured path and target", path, target),
	}
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
	hasGitHubProject := doctorHasGitHubProject(ctx, cfg, deps)
	requiresRuntimeToken := doctorRequiresRuntimeGitHubToken(ctx, cfg, deps)
	if cfg != nil && !hasGitHubProject {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorWarn,
			Detail: "no GitHub tracker projects configured; token scope check skipped",
			Hint:   "Add a GitHub project before relying on GitHub token preflight checks.",
		}
	}
	if token.Value == "" && !requiresRuntimeToken && hasGitHubProject {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorOK,
			Detail: "GitHub App credentials configured; installation permissions checked per project",
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
			Hint:   githubAuthHint,
		}
	}
	if len(scopes) == 0 {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorOK,
			Detail: fmt.Sprintf("%s did not expose classic OAuth scopes; treating as fine-grained or resource-scoped token and relying on operation checks", source),
		}
	}
	requiredScopes := doctorRequiredGitHubScopes(ctx, cfg, deps)
	missing := missingGitHubScopes(scopes, requiredScopes)
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
		Detail: fmt.Sprintf("%s has classic PAT scopes: %s; operation checks still verify resource access", source, strings.Join(requiredScopes, ", ")),
	}
}

func doctorHasGitHubProject(ctx context.Context, cfg *globalconfig.Config, deps doctorDeps) bool {
	if cfg != nil {
		for _, project := range cfg.Projects {
			workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
			if err != nil || workflow.Config.Tracker.Kind != workflowconfig.TrackerGitHub {
				continue
			}
			return true
		}
	}
	return false
}

func doctorRequiresRuntimeGitHubToken(ctx context.Context, cfg *globalconfig.Config, deps doctorDeps) bool {
	if cfg == nil {
		return true
	}
	for _, project := range cfg.Projects {
		workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
		if err != nil || workflow.Config.Tracker.Kind != workflowconfig.TrackerGitHub {
			continue
		}
		if trackerHasGitHubAppCredentials(workflow.Config.Tracker, deps.lookupEnv) {
			continue
		}
		return true
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

func doctorRequiredGitHubScopes(ctx context.Context, cfg *globalconfig.Config, deps doctorDeps) []string {
	if cfg == nil {
		return append([]string{}, requiredProjectV2GitHubScopes...)
	}
	required := []string{}
	add := func(scopes []string) {
		for _, scope := range scopes {
			scope = strings.TrimSpace(scope)
			if scope == "" || doctorStringSliceContains(required, scope) {
				continue
			}
			required = append(required, scope)
		}
	}
	for _, project := range cfg.Projects {
		workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
		if err != nil || workflow.Config.Tracker.Kind != workflowconfig.TrackerGitHub {
			continue
		}
		if trackerHasGitHubAppCredentials(workflow.Config.Tracker, deps.lookupEnv) {
			continue
		}
		switch workflow.Config.Tracker.GitHubStatusSource {
		case workflowconfig.GitHubStatusSourceIssueField:
			add(requiredIssueFieldGitHubScopes)
		case workflowconfig.GitHubStatusSourceLabel:
			add(requiredLabelGitHubScopes)
		default:
			add(requiredProjectV2GitHubScopes)
		}
	}
	if len(required) == 0 {
		return append([]string{}, requiredProjectV2GitHubScopes...)
	}
	return required
}

func loadDoctorProjectWorkflow(ctx context.Context, project globalconfig.Project, deps doctorDeps) (workflowconfig.Workflow, error) {
	if strings.TrimSpace(project.WorkflowRef) == "" {
		return deps.loadWorkflow(project.Workflow)
	}
	return projectpkg.LoadWorkflowContext(ctx, project)
}

func doctorStringSliceContains(values []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func missingGitHubScopes(scopes []string, requiredScopes []string) []string {
	have := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope != "" {
			have[scope] = struct{}{}
		}
	}

	var missing []string
	for _, scope := range requiredScopes {
		if !hasEffectiveGitHubScope(have, scope) {
			missing = append(missing, scope)
		}
	}
	return missing
}

func hasEffectiveGitHubScope(scopes map[string]struct{}, scope string) bool {
	if _, ok := scopes[scope]; ok {
		return true
	}
	return scope == "read:project" && hasGitHubProjectScope(scopes)
}

func hasGitHubProjectScope(scopes map[string]struct{}) bool {
	_, ok := scopes["project"]
	return ok
}

func checkDoctorServerPort(ctx context.Context, cfg BootConfig, deps doctorDeps) doctorCheck {
	if ctx == nil {
		ctx = context.Background()
	}
	deps = deps.withDefaults()
	addr := serverAddr(cfg)
	listener, err := deps.listen("tcp", addr)
	if err != nil {
		check := doctorCheck{
			Name:   "Server port",
			Status: doctorFail,
			Detail: fmt.Sprintf("%s is not available for pre-start bind: %v", addr, err),
			Hint:   "Stop the process using the port or pass --port with an available value.",
		}
		if !doctorListenErrIndicatesOccupied(err) || doctorServerPort(cfg) == 0 {
			return check
		}
		probe, probeErr := probeDoctorHealth(ctx, cfg, deps)
		if probeErr != nil {
			check.Detail = fmt.Sprintf("%s is occupied for pre-start bind; health probe %s %v", addr, probe.URL, probeErr)
			return check
		}
		return doctorCheck{
			Name:   "Server port",
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s is occupied for pre-start bind; health probe %s found healthy Detent instance (status %s, mode %s)", addr, probe.URL, probe.Health.Status, probe.Health.Mode),
			Hint:   "No action is needed if doctor is checking the live instance; stop Detent before a clean pre-start availability check.",
		}
	}
	if err := listener.Close(); err != nil {
		return doctorCheck{
			Name:   "Server port",
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s was available for pre-start bind, but close failed: %v", addr, err),
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
		Detail: addr + " is available for pre-start bind",
	}
}

type doctorHealthProbe struct {
	URL    string
	Health doctorHealthResponse
}

type doctorHealthResponse struct {
	Status string            `json:"status"`
	Mode   string            `json:"mode"`
	Checks map[string]string `json:"checks"`
}

func probeDoctorHealth(ctx context.Context, cfg BootConfig, deps doctorDeps) (doctorHealthProbe, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	url := doctorHealthProbeURL(cfg)
	probe := doctorHealthProbe{URL: url}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return probe, fmt.Errorf("could not be built: %w", err)
	}
	resp, err := deps.httpDo(req)
	if err != nil {
		return probe, fmt.Errorf("could not be reached: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return probe, fmt.Errorf("returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&probe.Health); err != nil {
		return probe, fmt.Errorf("did not return Detent health: %w", err)
	}
	probe.Health.Status = strings.TrimSpace(probe.Health.Status)
	probe.Health.Mode = strings.TrimSpace(probe.Health.Mode)
	if probe.Health.Mode == "" || !doctorHealthHasDetentChecks(probe.Health.Checks) {
		return probe, errors.New("did not return Detent health")
	}
	if probe.Health.Status != "ok" {
		return probe, fmt.Errorf("did not report healthy status: status %s, mode %s", probe.Health.Status, probe.Health.Mode)
	}
	return probe, nil
}

func doctorHealthHasDetentChecks(checks map[string]string) bool {
	if checks == nil {
		return false
	}
	for _, key := range doctorHealthCheckKeys {
		if _, ok := checks[key]; !ok {
			return false
		}
	}
	return true
}

func doctorHealthProbeURL(cfg BootConfig) string {
	return "http://" + net.JoinHostPort(doctorHealthProbeHost(cfg.Host), strconv.Itoa(doctorServerPort(cfg))) + "/health"
}

func doctorHealthProbeHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		host = defaultWebHost
	}
	host = unbracketIPv6Host(host)
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip.IsUnspecified() {
		if ip.To4() != nil {
			return "127.0.0.1"
		}
		return "::1"
	}
	return host
}

func doctorServerPort(cfg BootConfig) int {
	if cfg.Port != nil {
		return *cfg.Port
	}
	return defaultWebPort
}

func doctorListenErrIndicatesOccupied(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return errors.Is(err, syscall.EADDRINUSE) ||
		strings.Contains(message, "address already in use") ||
		strings.Contains(message, "only one usage of each socket address")
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
	if d.gitWorkTree == nil {
		d.gitWorkTree = defaults.gitWorkTree
	}
	if d.gitRemoteURL == nil {
		d.gitRemoteURL = defaults.gitRemoteURL
	}
	if d.autoPromoteConnector == nil {
		d.autoPromoteConnector = defaults.autoPromoteConnector
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
		gitWorkTree:          defaultGitWorkTree,
		gitRemoteURL:         defaultGitRemoteURL,
		autoPromoteConnector: defaultDoctorAutoPromoteConnector,
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
