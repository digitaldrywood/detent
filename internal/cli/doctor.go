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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/codex"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/healthnotify"
	"github.com/digitaldrywood/detent/internal/telemetry"
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
	Name                       string                                     `json:"name"`
	Status                     doctorStatus                               `json:"status"`
	Detail                     string                                     `json:"detail"`
	Hint                       string                                     `json:"hint,omitempty"`
	BinaryResolution           *doctorBinaryResolution                    `json:"binary_resolution,omitempty"`
	WorkflowSkillSuggestions   []doctorWorkflowSkillSuggestion            `json:"workflow_skill_suggestions,omitempty"`
	AutoPromoteCandidates      []doctorAutoPromoteCandidateDiagnostic     `json:"auto_promote_candidates,omitempty"`
	BlockedRecoveryCandidates  []doctorBlockedRecoveryCandidateDiagnostic `json:"blocked_recovery_candidates,omitempty"`
	PermanentlyHeldRecoveries  []doctorBlockedRecoveryCandidateDiagnostic `json:"permanently_held_recoveries,omitempty"`
	BlockedWithoutRecovery     []doctorBlockedRecoveryCandidateDiagnostic `json:"blocked_without_recovery,omitempty"`
	BackendCapacity            []doctorBackendCapacityDiagnostic          `json:"backend_capacity,omitempty"`
	ArtifactGateConvergence    []doctorArtifactGateConvergenceDiagnostic  `json:"artifact_gate_convergence,omitempty"`
	ValidatorFailures          []doctorValidatorFailureDiagnostic         `json:"validator_failures,omitempty"`
	Routines                   []doctorRoutineDiagnostic                  `json:"routines,omitempty"`
	BacklogAdmission           *doctorAdmissionDiagnostic                 `json:"backlog_admission,omitempty"`
	OverloadRetriesLastHour    int                                        `json:"overload_retries_last_hour,omitempty"`
	DependencyCapabilities     []connector.DependencyCapability           `json:"dependency_capabilities,omitempty"`
	StalenessWarnings          []telemetry.StalenessWarning               `json:"staleness_warnings,omitempty"`
	StrandedIssues             []telemetry.StrandedIssue                  `json:"stranded_active_issues,omitempty"`
	DispatchStalls             []telemetry.DispatchStatus                 `json:"dispatch_stalls,omitempty"`
	HealthNotificationFailures []healthnotify.Failure                     `json:"health_notification_failures,omitempty"`
	UntrackedIssues            []doctorStatusDriftIssueDiagnostic         `json:"untracked_issues,omitempty"`
	OpenTerminalIssues         []doctorStatusDriftIssueDiagnostic         `json:"open_terminal_issues,omitempty"`
	ClosedActiveIssues         []doctorStatusDriftIssueDiagnostic         `json:"closed_active_issues,omitempty"`
	AuthorizationAttention     []doctorAuthorizationAttentionDiagnostic   `json:"authorization_attention,omitempty"`
	OwnershipAttention         []doctorOwnershipAttentionDiagnostic       `json:"ownership_attention,omitempty"`
	OrphanedAgentProcesses     *telemetry.OrphanedAgentProcesses          `json:"orphaned_agent_processes,omitempty"`
	ParkReviews                []doctorParkReviewDiagnostic               `json:"park_reviews,omitempty"`
	ProjectDefinition          *doctorProjectDefinitionDiagnostic         `json:"project_definition,omitempty"`
	Capabilities               *doctorCapabilityReport                    `json:"capabilities,omitempty"`
	WorkflowOptimization       doctorWorkflowOptimizationReport           `json:"-"`
	ConcurrencyHistory         []doctorConcurrencyDiagnostic              `json:"concurrency_history,omitempty"`
	RedispatchLoops            []doctorRedispatchDiagnostic               `json:"redispatch_loops,omitempty"`
}

type doctorProjectDefinitionDiagnostic struct {
	ProjectID         string   `json:"project_id"`
	Layout            string   `json:"layout"`
	Revision          string   `json:"revision,omitempty"`
	WorkflowPath      string   `json:"workflow_path"`
	ConfigPath        string   `json:"config_path"`
	LocalWorkflowPath string   `json:"local_workflow_path,omitempty"`
	LocalConfigPath   string   `json:"local_config_path,omitempty"`
	LegacyKeys        []string `json:"legacy_keys,omitempty"`
	LocalLegacyKeys   []string `json:"local_legacy_keys,omitempty"`
	FixCommand        string   `json:"fix_command,omitempty"`
	RuntimeLayout     string   `json:"runtime_layout,omitempty"`
	RuntimeRevision   string   `json:"runtime_revision,omitempty"`
	Stale             bool     `json:"stale,omitempty"`
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
	Name     string
	Current  func() string
	Freeze   func() doctorCheckSnapshot
	Progress <-chan struct{}
	Run      func(context.Context) []doctorCheck
}

type doctorCheckSnapshot struct {
	Current string
	Checks  []doctorCheck
}

type doctorCheckResult struct {
	Index  int
	Checks []doctorCheck
}

type doctorCheckTimer interface {
	C() <-chan time.Time
	Reset(time.Duration) bool
	Stop() bool
}

type systemDoctorCheckTimer struct {
	timer *time.Timer
}

type doctorCheckProgress struct {
	mu      sync.Mutex
	current string
	checks  []doctorCheck
	frozen  bool
	updates chan struct{}
}

func newDoctorCheckProgress() *doctorCheckProgress {
	return &doctorCheckProgress{updates: make(chan struct{}, 1)}
}

func (p *doctorCheckProgress) Set(current string, checks []doctorCheck) {
	p.mu.Lock()
	if p.frozen {
		p.mu.Unlock()
		return
	}
	p.current = strings.TrimSpace(current)
	p.checks = append(p.checks[:0], checks...)
	p.mu.Unlock()

	p.Pulse()
}

func (p *doctorCheckProgress) Pulse() {
	p.mu.Lock()
	frozen := p.frozen
	p.mu.Unlock()
	if frozen {
		return
	}

	select {
	case p.updates <- struct{}{}:
	default:
	}
}

func (p *doctorCheckProgress) Current() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

func (p *doctorCheckProgress) Freeze() doctorCheckSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frozen = true
	return doctorCheckSnapshot{
		Current: p.current,
		Checks:  append([]doctorCheck(nil), p.checks...),
	}
}

func (p *doctorCheckProgress) Updates() <-chan struct{} {
	return p.updates
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
	WorkflowTokenThreshold    int
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
	resolveCommandOnPath func(string, string) (string, error)
	resolveCommandInDir  func(context.Context, string, []string, string) (string, error)
	runCommandInDir      func(context.Context, string, []string, string, ...string) error
	codexInitialize      func(context.Context, string, []string) error
	codexAccount         func(context.Context, workflowconfig.AgentBackend, []string) (codex.Account, error)
	httpDo               func(*http.Request) (*http.Response, error)
	githubScopes         func(context.Context, string) ([]string, error)
	githubReadiness      doctorGitHubReadinessFunc
	githubMergeSettings  func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryMergeSettings, error)
	githubRepositoryInfo func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryInfo, error)
	githubLabels         func(context.Context, workflowconfig.Config, string) ([]string, error)
	ghAuthToken          func(context.Context) (string, error)
	listen               func(string, string) (net.Listener, error)
	openSQLite           func(context.Context, string) (doctorStore, error)
	openSQLiteReadOnly   func(context.Context, string) (doctorTelemetryStore, error)
	gitWorkTree          func(context.Context, string) error
	gitWorktrees         func(context.Context, string) ([]doctorGitWorktree, error)
	gitRemoteURL         func(context.Context, string) (string, error)
	gitTracked           func(context.Context, string) (bool, error)
	workflowSourcePolicy doctorWorkflowSourcePolicyFunc
	scheduleOwnership    doctorScheduleOwnershipProbe
	autoPromoteConnector func(workflowconfig.Config) (doctorAutoPromoteConnector, error)
	proposalConnector    func(workflowconfig.Config) (doctorWorkflowProposalConnector, error)
	modelProbe           func(context.Context, doctorRouteModelProbeRequest) error
	executable           func() (string, error)
	shipSkillProbe       func(string) (doctorShipSkill, error)
	now                  func() time.Time
	workflowCache        *doctorWorkflowCache
	pauseProjects        []globalconfig.Project
	pauseGitHubToken     string
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
	workflowTokenThreshold := doctorWorkflowDefaultTokenThreshold
	workflowProposalThreshold := doctorWorkflowProposalDefaultThreshold
	strict := false
	startupPreflight := false
	projectID := ""
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Run preflight health checks",
		Example:      "detent doctor --config ~/.config/detent/global.yaml\n  detent doctor --project example --port 0",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if workflowTokenThreshold <= 0 {
				return errors.New("--workflow-token-threshold must be greater than zero")
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			progressOut := cmd.OutOrStdout()
			if out.IsJSON() {
				progressOut = cmd.ErrOrStderr()
			}
			config := doctorConfig{
				ConfigPath:                derefString(configPath),
				Host:                      derefString(host),
				ProjectID:                 projectID,
				Output:                    progressOut,
				CheckTimeout:              timeout,
				Build:                     opts.build,
				AllowWriteProbes:          allowWriteProbes,
				WorkflowDiff:              workflowDiff || workflowWrite,
				WorkflowTokenThreshold:    workflowTokenThreshold,
				WorkflowProposalThreshold: workflowProposalThreshold,
				WorkflowProposeIssues:     workflowProposeIssues,
				Flags: runtimeFlags{
					Env:      runtimeStringFlag{Value: derefString(env), Set: flagChanged(cmd, "env")},
					LogLevel: runtimeStringFlag{Value: derefString(logLevel), Set: flagChanged(cmd, "log-level")},
					Port:     runtimeIntFlag{Value: derefInt(port, -1), Set: flagChanged(cmd, "port")},
				},
			}
			var report doctorReport
			if startupPreflight {
				report = runDoctorStartupPreflight(cmd.Context(), config, opts, deps)
			} else {
				report = runDoctor(cmd.Context(), config, opts, deps)
			}
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
	cmd.Flags().IntVar(&workflowTokenThreshold, "workflow-token-threshold", doctorWorkflowDefaultTokenThreshold, "warn when a WORKFLOW.md prompt body exceeds this estimated token count")
	cmd.Flags().IntVar(&workflowProposalThreshold, "proposal-threshold", doctorWorkflowProposalDefaultThreshold, "minimum repeated signal count before emitting a governed self-improvement proposal")
	cmd.Flags().BoolVar(&workflowProposeIssues, "propose-issues", false, "create governed backlog issue proposals for repeated workflow optimization signals")
	cmd.Flags().BoolVar(&strict, "strict", false, "fail when checks warn or workflow optimization findings exist")
	cmd.Flags().BoolVar(&startupPreflight, "startup-preflight", false, "limit checks to candidate startup compatibility")
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
		check := checkDoctorConfigReload(ctx, *global, opts.runCommand)
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

	liveBoot := doctorLiveBoot(boot, global)
	binaryEnvironment := resolveDoctorBinaryEnvironment(ctx, resolution, liveBoot, deps)
	jobs := []doctorCheckJob{}
	if global != nil {
		globalConfig := *global
		workflowDriftBoot := liveBoot
		githubToken := runtime.GitHubToken
		jobs = append(jobs, doctorCheckJob{
			Name: "Update checking",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorUpdateRuntime(jobCtx, globalConfig.Update, boot, deps)}
			},
		})
		if projectScopeCheck == nil {
			jobs = append(jobs, doctorProjectCheckJobs(globalConfig, deps, githubToken, cfg.AllowWriteProbes, cfg.WorkflowTokenThreshold)...)
			jobs = append(jobs, doctorCheckJob{
				Name: "Workflow runtime drift",
				Run: func(jobCtx context.Context) []doctorCheck {
					return checkDoctorWorkflowDrift(jobCtx, globalConfig, workflowDriftBoot, deps)
				},
			})
		}
		jobs = append(jobs, doctorCheckJob{
			Name: "Billing auth",
			Run: func(jobCtx context.Context) []doctorCheck {
				return checkDoctorMeteredBillingAuth(jobCtx, globalConfig, deps, binaryEnvironment)
			},
		})
		jobs = append(jobs, doctorCheckJob{
			Name: "Lessons",
			Run: func(jobCtx context.Context) []doctorCheck {
				return checkDoctorLessonCaptures(jobCtx, globalConfig, deps)
			},
		})
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
		jobs = append(jobs, doctorCheckJob{
			Name: doctorAgentPoolsCheckName,
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorAgentPools(jobCtx, resolution, globalConfig, deps)}
			},
		})
	}
	jobs = append(jobs,
		doctorCheckJob{
			Name: "Remote Detent service",
			Run: func(jobCtx context.Context) []doctorCheck {
				return checkDoctorDetentService(jobCtx, liveBoot, cfg.Build, deps)
			},
		},
		doctorCheckJob{
			Name: "Instance lock",
			Run: func(_ context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorInstanceLock(resolution)}
			},
		},
		doctorCheckJob{
			Name: "SQLite database",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorSQLite(jobCtx, resolution, deps)}
			},
		},
		doctorCheckJob{
			Name: "Daily budget attribution",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorDailyBudgetAccuracy(jobCtx, resolution, deps, time.Now())}
			},
		},
		doctorCheckJob{
			Name: "Budget overrides",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorBudgetOverrides(jobCtx, resolution, cfg.ProjectID, time.Now().UTC(), deps)}
			},
		},
		doctorCheckJob{
			Name: "Backend capacity",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorBackendCapacity(jobCtx, resolution, boot, cfg.ProjectID, deps, time.Now())}
			},
		},
		doctorCheckJob{
			Name: "Fleet staleness",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorFleetStaleness(jobCtx, boot, cfg.ProjectID, deps)}
			},
		},
		doctorCheckJob{
			Name: "Health notification delivery",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorHealthNotificationDelivery(jobCtx, boot, cfg.ProjectID, deps)}
			},
		},
		doctorCheckJob{
			Name: "Stranded active work",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorStrandedActive(jobCtx, boot, cfg.ProjectID, deps)}
			},
		},
		doctorCheckJob{
			Name: "Dispatch stalls",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorDispatchStalls(jobCtx, boot, cfg.ProjectID, deps)}
			},
		},
		doctorCheckJob{
			Name: "Artifact gate convergence",
			Run: func(jobCtx context.Context) []doctorCheck {
				return []doctorCheck{checkDoctorArtifactGateConvergence(jobCtx, resolution, cfg.ProjectID, deps)}
			},
		},
	)
	jobs = append(jobs, doctorAgentBinaryCheckJobs(ctx, global, deps, binaryEnvironment)...)
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
				return []doctorCheck{checkDoctorGit(jobCtx, deps, binaryEnvironment)}
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

func doctorLiveBoot(boot BootConfig, cfg *globalconfig.Config) BootConfig {
	if doctorServerPort(boot) != 0 {
		return boot
	}
	port := defaultWebPort
	if cfg != nil && cfg.Port != nil {
		port = *cfg.Port
	}
	boot.Port = &port
	return boot
}

func checkDoctorUpdateRuntime(ctx context.Context, cfg globalconfig.Update, boot BootConfig, deps doctorDeps) doctorCheck {
	status, ok := readDoctorUpdateStatus(ctx, boot, deps)
	if !ok {
		return checkDoctorUpdate(cfg, nil)
	}
	return checkDoctorUpdate(cfg, &status)
}

func checkDoctorUpdate(cfg globalconfig.Update, runtimeStatus *telemetry.Update) doctorCheck {
	detail := doctorUpdateStatusDetail(runtimeStatus)
	if !cfg.AutoCheckEnabled {
		return doctorCheck{
			Name:   "Update checking",
			Status: doctorWarn,
			Detail: "suggestion: update.auto_check_enabled is disabled; you are responsible for noticing new Detent releases; " + detail,
			Hint:   "Set update.auto_check_enabled: true in global.yaml to check automatically while keeping update.auto_apply_enabled off for notification-only behavior.",
		}
	}
	mode := "notify only"
	if cfg.AutoApplyEnabled {
		mode = "auto-apply and graceful restart"
	}
	return doctorCheck{
		Name:   "Update checking",
		Status: doctorOK,
		Detail: fmt.Sprintf("enabled every %d hours (%s); %s", cfg.NormalizedCheckIntervalHours(), mode, detail),
	}
}

func readDoctorUpdateStatus(ctx context.Context, boot BootConfig, deps doctorDeps) (telemetry.Update, bool) {
	port := defaultWebPort
	if boot.Port != nil {
		port = *boot.Port
	}
	if port <= 0 || deps.httpDo == nil {
		return telemetry.Update{}, false
	}
	host := unbracketIPv6Host(strings.TrimSpace(boot.Host))
	switch host {
	case "", "0.0.0.0", "::":
		host = defaultWebHost
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, strconv.Itoa(port))+"/health", nil)
	if err != nil {
		return telemetry.Update{}, false
	}
	resp, err := deps.httpDo(req)
	if err != nil {
		return telemetry.Update{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return telemetry.Update{}, false
	}
	var payload struct {
		Update telemetry.Update `json:"update"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.Update.IsZero() {
		return telemetry.Update{}, false
	}
	return payload.Update, true
}

func doctorUpdateStatusDetail(status *telemetry.Update) string {
	if status == nil {
		return "last check: unavailable; last applied version: unavailable; next check: unavailable"
	}
	lastCheck := "never"
	if status.LastCheckAt != nil {
		lastCheck = status.LastCheckAt.Format(time.RFC3339)
	}
	lastApplied := strings.TrimSpace(status.LastAppliedVersion)
	if lastApplied == "" {
		lastApplied = "none"
	}
	nextCheck := "not scheduled"
	if status.NextCheckAt != nil {
		nextCheck = status.NextCheckAt.Format(time.RFC3339)
	}
	return "last check: " + lastCheck + "; last applied version: " + lastApplied + "; next check: " + nextCheck
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
	return runDoctorCheckWithTimer(ctx, job, timeout, &systemDoctorCheckTimer{timer: time.NewTimer(timeout)})
}

func runDoctorCheckWithTimer(ctx context.Context, job doctorCheckJob, timeout time.Duration, timer doctorCheckTimer) []doctorCheck {
	checkCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	defer timer.Stop()

	done := make(chan []doctorCheck, 1)
	go func() {
		done <- job.Run(checkCtx)
	}()

	for {
		select {
		case checks := <-done:
			return checks
		case <-job.Progress:
			resetDoctorCheckTimer(timer, timeout)
		case <-timer.C():
			snapshot := freezeDoctorCheck(job)
			cancel()
			return doctorTimedOutChecks(job.Name, snapshot, timeout, context.DeadlineExceeded)
		case <-ctx.Done():
			snapshot := freezeDoctorCheck(job)
			cancel()
			return doctorTimedOutChecks(job.Name, snapshot, timeout, ctx.Err())
		}
	}
}

func (t *systemDoctorCheckTimer) C() <-chan time.Time {
	return t.timer.C
}

func (t *systemDoctorCheckTimer) Reset(timeout time.Duration) bool {
	return t.timer.Reset(timeout)
}

func (t *systemDoctorCheckTimer) Stop() bool {
	return t.timer.Stop()
}

func freezeDoctorCheck(job doctorCheckJob) doctorCheckSnapshot {
	if job.Freeze != nil {
		return job.Freeze()
	}
	snapshot := doctorCheckSnapshot{}
	if job.Current != nil {
		snapshot.Current = strings.TrimSpace(job.Current())
	}
	return snapshot
}

func doctorTimedOutChecks(name string, snapshot doctorCheckSnapshot, timeout time.Duration, err error) []doctorCheck {
	return append(snapshot.Checks, doctorCheck{
		Name:   name,
		Status: doctorFail,
		Detail: doctorTimeoutDetail(name, snapshot.Current, timeout, err),
		Hint:   doctorTimeoutHint(),
	})
}

func resetDoctorCheckTimer(timer doctorCheckTimer, timeout time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C():
		default:
		}
	}
	timer.Reset(timeout)
}

func doctorTimeoutDetail(name string, current string, timeout time.Duration, err error) string {
	current = strings.TrimSpace(current)
	if current != "" && current != strings.TrimSpace(name) {
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
		if check.Capabilities != nil {
			for _, capability := range check.Capabilities.Capabilities {
				if _, err := fmt.Fprintf(out, "%-5s  %-28s  %s\n", "", strings.ToUpper(string(capability.State))+" "+capability.Name, capability.Detail); err != nil {
					return err
				}
			}
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
		read := opts.readDoctor
		if read == nil {
			read = opts.read
		}
		cfg, err = read(resolution.Path)
	} else {
		var skippedProjects []string
		readProject := opts.readDoctorProject
		if readProject == nil {
			readProject = opts.readProject
		}
		cfg, skippedProjects, err = readProject(resolution.Path, projectID)
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
	if d.resolveCommandOnPath == nil {
		d.resolveCommandOnPath = defaults.resolveCommandOnPath
	}
	if d.resolveCommandInDir == nil {
		d.resolveCommandInDir = defaults.resolveCommandInDir
	}
	if d.runCommandInDir == nil {
		d.runCommandInDir = defaults.runCommandInDir
	}
	if d.codexInitialize == nil {
		d.codexInitialize = defaults.codexInitialize
	}
	if d.codexAccount == nil {
		d.codexAccount = defaults.codexAccount
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
	if d.githubMergeSettings == nil {
		d.githubMergeSettings = defaults.githubMergeSettings
	}
	if d.githubRepositoryInfo == nil {
		d.githubRepositoryInfo = defaults.githubRepositoryInfo
	}
	if d.githubLabels == nil {
		d.githubLabels = defaults.githubLabels
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
	if d.gitWorktrees == nil {
		d.gitWorktrees = defaults.gitWorktrees
	}
	if d.gitRemoteURL == nil {
		d.gitRemoteURL = defaults.gitRemoteURL
	}
	if d.gitTracked == nil {
		d.gitTracked = defaults.gitTracked
	}
	if d.workflowSourcePolicy == nil {
		d.workflowSourcePolicy = defaults.workflowSourcePolicy
	}
	if d.scheduleOwnership == nil {
		d.scheduleOwnership = defaults.scheduleOwnership
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
	if d.shipSkillProbe == nil {
		d.shipSkillProbe = defaults.shipSkillProbe
	}
	if d.now == nil {
		d.now = defaults.now
	}
	if d.workflowCache == nil {
		d.workflowCache = newDoctorWorkflowCache()
	}
	return d
}

func defaultDoctorDeps() doctorDeps {
	return doctorDeps{
		loadWorkflow:         workflowconfig.LoadWorkflow,
		lookupEnv:            os.Getenv,
		resolveCommandOnPath: resolveDoctorCommandOnPath,
		resolveCommandInDir:  resolveDoctorCommandInDir,
		runCommandInDir:      runDoctorCommandInDir,
		codexInitialize:      probeDoctorCodexInitialize,
		codexAccount:         probeDoctorCodexAccount,
		httpDo:               defaultDoctorHTTPDo,
		githubScopes:         defaultGitHubScopes,
		githubReadiness:      ghconnector.CheckReadiness,
		githubMergeSettings:  defaultDoctorGitHubMergeSettings,
		githubRepositoryInfo: defaultDoctorGitHubRepositoryInfo,
		githubLabels:         defaultDoctorGitHubRepositoryLabels,
		ghAuthToken:          defaultGHAuthToken,
		listen:               net.Listen,
		openSQLite:           openDoctorSQLite,
		openSQLiteReadOnly:   openDoctorSQLiteReadOnly,
		gitWorkTree:          defaultGitWorkTree,
		gitWorktrees:         defaultDoctorGitWorktrees,
		gitRemoteURL:         defaultGitRemoteURL,
		gitTracked:           defaultGitTracked,
		workflowSourcePolicy: defaultDoctorWorkflowSourcePolicy,
		scheduleOwnership:    defaultDoctorScheduleOwnership,
		autoPromoteConnector: defaultDoctorAutoPromoteConnector,
		proposalConnector:    defaultDoctorProposalConnector,
		modelProbe:           defaultDoctorRouteModelProbe,
		executable:           os.Executable,
		shipSkillProbe:       probeDoctorShipSkill,
		now:                  time.Now,
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

func resolveDoctorCommandOnPath(pathValue string, executable string) (string, error) {
	if runtime.GOOS == "windows" {
		dir, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return resolveDoctorWindowsCommand(dir, doctorCommandEnvironment(os.Environ(), []string{"PATH=" + pathValue}), executable)
	}
	if strings.ContainsRune(executable, os.PathSeparator) {
		if doctorExecutablePath(executable) {
			return executable, nil
		}
		return "", fmt.Errorf("executable %q not found", executable)
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, executable)
		if doctorExecutablePath(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found on configured PATH", executable)
}

func doctorExecutablePath(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func resolveDoctorCommandInDir(ctx context.Context, dir string, environment []string, executable string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()

	if runtime.GOOS == "windows" {
		return resolveDoctorWindowsCommand(dir, doctorCommandEnvironment(os.Environ(), environment), executable)
	}
	cmd := exec.CommandContext(commandCtx, "sh", "-c", `command -v -- "$1"`, "detent-doctor", executable) // #nosec G204 -- executable is passed as a positional shell parameter.
	cmd.Dir = dir
	cmd.Env = doctorCommandEnvironment(os.Environ(), environment)
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return "", commandCtx.Err()
	}
	resolved := strings.TrimSpace(string(output))
	if err != nil {
		if resolved != "" {
			return "", fmt.Errorf("%w: %s", err, resolved)
		}
		return "", err
	}
	if resolved == "" {
		return "", errors.New("command -v returned no result")
	}
	return resolved, nil
}

func resolveDoctorWindowsCommand(dir string, environment []string, executable string) (string, error) {
	pathExt := doctorEnvironmentValue(environment, "PATHEXT", true)
	if strings.ContainsAny(executable, `/\\`) {
		if !filepath.IsAbs(executable) {
			executable = filepath.Join(dir, executable)
		}
		return firstDoctorWindowsCommand(executable, pathExt)
	}

	pathValue := doctorEnvironmentValue(environment, "PATH", true)
	for _, searchDir := range strings.Split(pathValue, ";") {
		if searchDir == "" {
			searchDir = dir
		} else if !filepath.IsAbs(searchDir) {
			searchDir = filepath.Join(dir, searchDir)
		}
		if resolved, err := firstDoctorWindowsCommand(filepath.Join(searchDir, executable), pathExt); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("executable %q not found on configured PATH", executable)
}

func firstDoctorWindowsCommand(path string, pathExt string) (string, error) {
	candidates := []string{path}
	extensions := strings.Split(pathExt, ";")
	if strings.TrimSpace(pathExt) == "" {
		extensions = []string{".COM", ".EXE", ".BAT", ".CMD"}
	}
	for _, extension := range extensions {
		extension = strings.TrimSpace(extension)
		if extension == "" {
			continue
		}
		if !strings.HasPrefix(extension, ".") {
			extension = "." + extension
		}
		candidates = append(candidates, path+extension)
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found", path)
}

func runDoctorCommandInDir(ctx context.Context, dir string, environment []string, path string, args ...string) error {
	commandCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, path, args...) // #nosec G204 -- doctor runs an operator-configured gate through a PATH-resolved executable.
	cmd.Dir = dir
	cmd.Env = doctorCommandEnvironment(os.Environ(), environment)
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

func doctorCommandEnvironment(base []string, overrides []string) []string {
	return doctorCommandEnvironmentForOS(base, overrides, runtime.GOOS)
}

func doctorCommandEnvironmentForOS(base []string, overrides []string, goos string) []string {
	environment := append([]string(nil), base...)
	for _, override := range overrides {
		name, _, ok := strings.Cut(override, "=")
		if !ok {
			continue
		}
		values := doctorEnvironmentValues(environment)
		if goos == "windows" {
			values = doctorEnvironmentValuesFolded(environment)
		}
		override = name + "=" + os.Expand(strings.TrimPrefix(override, name+"="), func(key string) string {
			if goos == "windows" {
				key = strings.ToUpper(key)
			}
			return values[key]
		})
		filtered := environment[:0]
		for _, entry := range environment {
			candidate, _, candidateOK := strings.Cut(entry, "=")
			matches := candidateOK && candidate == name
			if goos == "windows" {
				matches = candidateOK && strings.EqualFold(candidate, name)
			}
			if !matches {
				filtered = append(filtered, entry)
			}
		}
		environment = append(filtered, override)
	}
	return environment
}

func doctorEnvironmentValuesFolded(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(name)] = value
		}
	}
	return values
}

func doctorEnvironmentValues(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	return values
}

func doctorEnvironmentValue(environment []string, name string, caseInsensitive bool) string {
	value := ""
	for _, entry := range environment {
		candidate, candidateValue, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		matches := candidate == name
		if caseInsensitive {
			matches = strings.EqualFold(candidate, name)
		}
		if matches {
			value = candidateValue
		}
	}
	return value
}

func defaultGitWorkTree(ctx context.Context, path string) error {
	commandCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, "git", "-C", path, "rev-parse", "--is-inside-work-tree") // #nosec G204 -- the configured path is passed as a fixed git argument without a shell.
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

func defaultGitTracked(ctx context.Context, path string) (bool, error) {
	commandCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, "git", "-C", filepath.Dir(path), "ls-files", "--error-unmatch", "--", filepath.Base(path)) // #nosec G204 -- the local workflow path is passed as a git argument, not shell input.
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return false, commandCtx.Err()
	}
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	if detail := strings.TrimSpace(string(output)); detail != "" {
		return false, fmt.Errorf("check tracked file: %w: %s", err, detail)
	}
	return false, fmt.Errorf("check tracked file: %w", err)
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
