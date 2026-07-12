package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

var doctorHealthCheckKeys = []string{"hub", "store", "registry", "connector"}

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

func checkDoctorLocalSQLiteTracker(
	ctx context.Context,
	id string,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	deps doctorDeps,
) doctorCheck {
	name := "Project " + id + " local SQLite tracker"
	path := strings.TrimSpace(cfg.Tracker.LocalSQLite.Path)
	if path == "" {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "tracker.local_sqlite.path is not configured",
			Hint:   "Set tracker.local_sqlite.path to a SQLite database path for local work items.",
		}
	}
	resolved, err := resolveDoctorProjectPath(project, path)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", path, err),
			Hint:   "Set tracker.local_sqlite.path to an absolute, home-relative, or project workdir-relative path.",
		}
	}
	db, err := deps.openSQLite(ctx, resolved)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", resolved, err),
			Hint:   "Check local SQLite directory permissions and database integrity.",
		}
	}
	if err := db.Close(); err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s close failed: %v", resolved, err),
			Hint:   "Check for filesystem or SQLite errors, then rerun detent doctor.",
		}
	}
	return doctorCheck{
		Name:   name,
		Status: doctorOK,
		Detail: resolved + " is reachable",
	}
}

func checkDoctorFilesystemWorkspace(id string, cfg workflowconfig.Config) doctorCheck {
	name := "Project " + id + " filesystem workspace"
	root := strings.TrimSpace(cfg.Workspace.Root)
	if root == "" {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "workspace.root is not configured",
			Hint:   "Set workspace.root to a directory Detent can use for filesystem workspaces.",
		}
	}
	resolved, err := expandDoctorWorkspacePath(root)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", root, err),
			Hint:   "Set workspace.root to an absolute or home-relative directory.",
		}
	}
	if err := doctorPathDirectoryReady(resolved); err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("%s: %v", resolved, err),
			Hint:   "Create workspace.root or its parent directory and ensure Detent can write there.",
		}
	}
	if outputRoot := strings.TrimSpace(doctorFirstNonBlank(cfg.Workspace.OutputRoot, cfg.Deliverable.OutputRoot)); outputRoot != "" {
		resolvedOutput, err := expandDoctorWorkspacePath(outputRoot)
		if err != nil {
			return doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("%s: %v", outputRoot, err),
				Hint:   "Set workspace.output_root or deliverable.output_root to an absolute or home-relative directory.",
			}
		}
		if err := doctorPathDirectoryReady(resolvedOutput); err != nil {
			return doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("%s: %v", resolvedOutput, err),
				Hint:   "Create the artifact output directory or its parent and ensure Detent can write there.",
			}
		}
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: resolved + " is usable; artifact output " + resolvedOutput + " is usable",
		}
	}
	return doctorCheck{
		Name:   name,
		Status: doctorOK,
		Detail: resolved + " is usable",
	}
}

func resolveDoctorProjectPath(project globalconfig.Project, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(path) || path == "~" || strings.HasPrefix(path, "~/") {
		return expandDoctorWorkspacePath(path)
	}
	base := strings.TrimSpace(project.Workdir)
	if base == "" {
		return expandDoctorWorkspacePath(path)
	}
	expandedBase, err := expandDoctorWorkspacePath(base)
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(expandedBase, path)), nil
}

func doctorPathDirectoryReady(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return errors.New("path is not a directory")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	info, err = os.Stat(parent)
	if err != nil {
		return fmt.Errorf("parent directory is not available: %w", err)
	}
	if !info.IsDir() {
		return errors.New("parent path is not a directory")
	}
	return nil
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

func checkDoctorDailyBudgetAccuracy(ctx context.Context, resolution globalconfig.PathResolution, deps doctorDeps, now time.Time) doctorCheck {
	const name = "Daily budget attribution"
	if strings.TrimSpace(resolution.Path) == "" {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: "daily budget accuracy is unavailable because the global config path is unavailable"}
	}

	dbPath := filepath.Join(filepath.Dir(resolution.Path), "detent.db")
	db, err := deps.openSQLiteReadOnly(ctx, dbPath)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("daily budget accuracy is unavailable: %v", err)}
	}
	check := inspectDoctorDailyBudgetAccuracy(ctx, db, now)
	if err := db.Close(); err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("daily budget attribution database close failed: %v", err)}
	}
	return check
}

func inspectDoctorDailyBudgetAccuracy(ctx context.Context, db doctorTelemetryStore, now time.Time) doctorCheck {
	const name = "Daily budget attribution"
	var projectIDColumns int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('codex_sessions') WHERE name = 'project_id'").Scan(&projectIDColumns); err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("daily budget attribution could not be inspected: %v", err)}
	}
	if projectIDColumns == 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: "daily budget accuracy is degraded because the session project migration has not been applied",
			Hint:   "Restart Detent to apply the runtime store migration and project-registry backfill.",
		}
	}

	date := now.UTC().Format(time.DateOnly)
	var sessions int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_sessions WHERE substr(completed_at, 1, 10) = ? AND trim(COALESCE(project_id, '')) = ''`, date).Scan(&sessions); err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("daily budget attribution could not be inspected: %v", err)}
	}
	if sessions > 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: fmt.Sprintf("daily budget accuracy is degraded: %d completed session(s) today are unattributed and count toward every project", sessions),
			Hint:   "Ensure each project tracker repository matches session identifiers, then restart Detent to rerun the project-registry backfill.",
		}
	}
	return doctorCheck{Name: name, Status: doctorOK, Detail: "all completed sessions today have project attribution"}
}

func checkDoctorCodex(ctx context.Context, deps doctorDeps) doctorCheck {
	return checkDoctorBinary(ctx, deps, "codex", "codex binary", "--version", "Install Codex and ensure codex --version succeeds.")
}

func checkDoctorClaudeCode(ctx context.Context, deps doctorDeps) doctorCheck {
	return checkDoctorBinary(ctx, deps, "claude", "claude binary", "--version", "Install Claude Code and run `claude` once to log in (or set ANTHROPIC_API_KEY).")
}

func doctorAgentBinaryCheckJobs(ctx context.Context, cfg *globalconfig.Config, deps doctorDeps) []doctorCheckJob {
	kinds := doctorAgentBackendKinds(ctx, cfg, deps)
	jobs := make([]doctorCheckJob, 0, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case workflowconfig.AgentBackendCodex:
			jobs = append(jobs, doctorCodexBinaryCheckJob(deps))
		case workflowconfig.AgentBackendClaudeCode:
			jobs = append(jobs, doctorClaudeCodeBinaryCheckJob(deps))
		}
	}
	if len(jobs) == 0 {
		return []doctorCheckJob{doctorCodexBinaryCheckJob(deps)}
	}
	return jobs
}

func doctorAgentBackendKinds(ctx context.Context, cfg *globalconfig.Config, deps doctorDeps) []string {
	if cfg == nil {
		return nil
	}
	seen := map[string]struct{}{}
	kinds := []string{}
	add := func(kind string) {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			return
		}
		if _, ok := seen[kind]; ok {
			return
		}
		seen[kind] = struct{}{}
		kinds = append(kinds, kind)
	}
	for _, project := range cfg.Projects {
		workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
		if err != nil {
			continue
		}
		for _, backend := range workflow.Config.AgentBackendConfigs() {
			add(backend.Kind)
		}
	}
	return kinds
}

func doctorCodexBinaryCheckJob(deps doctorDeps) doctorCheckJob {
	return doctorCheckJob{
		Name: "codex binary",
		Run: func(jobCtx context.Context) []doctorCheck {
			return []doctorCheck{checkDoctorCodex(jobCtx, deps)}
		},
	}
}

func doctorClaudeCodeBinaryCheckJob(deps doctorDeps) doctorCheckJob {
	return doctorCheckJob{
		Name: "claude binary",
		Run: func(jobCtx context.Context) []doctorCheck {
			return []doctorCheck{checkDoctorClaudeCode(jobCtx, deps)}
		},
	}
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
		detail := fmt.Sprintf(
			"%s is occupied for pre-start bind; health probe %s found healthy Detent instance (status %s, mode %s)%s",
			addr,
			probe.URL,
			probe.Health.Status,
			probe.Health.Mode,
			doctorEnforcedBudgetDetail(probe.Health.Budgets),
		)
		return doctorCheck{
			Name:   "Server port",
			Status: doctorWarn,
			Detail: detail,
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
	Status  string               `json:"status"`
	Mode    string               `json:"mode"`
	Checks  map[string]string    `json:"checks"`
	Budgets []doctorHealthBudget `json:"budgets"`
}

type doctorHealthBudget struct {
	ProjectID      string  `json:"project_id"`
	Enabled        bool    `json:"enabled"`
	PerDayMaxUSD   float64 `json:"per_day_max_usd"`
	PerIssueMaxUSD float64 `json:"per_issue_max_usd"`
}

func doctorEnforcedBudgetDetail(budgets []doctorHealthBudget) string {
	if len(budgets) == 0 {
		return ""
	}

	parts := make([]string, 0, len(budgets))
	for _, budget := range budgets {
		parts = append(parts, fmt.Sprintf(
			"%s enabled=%t per_day_max_usd=%s per_issue_max_usd=%s",
			budget.ProjectID,
			budget.Enabled,
			strconv.FormatFloat(budget.PerDayMaxUSD, 'f', 2, 64),
			strconv.FormatFloat(budget.PerIssueMaxUSD, 'f', 2, 64),
		))
	}
	return "; enforced budget: " + strings.Join(parts, ", ")
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
