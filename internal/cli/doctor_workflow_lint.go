package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/gate"
)

const (
	doctorWorkflowWaitStatusLiveness = 15 * time.Minute
	doctorWorkflowWaitStatusWindow   = 24 * time.Hour
	doctorWorkflowCeilingWindow      = 24 * time.Hour
	doctorWorkflowCeilingSampleLimit = 50
	doctorWorkflowCeilingMinAttempts = 5
	doctorWorkflowCeilingShare       = 0.20
)

type doctorShipSkill struct {
	Version string
	Path    string
}

type doctorWaitStatusIncident struct {
	Issue       string
	Lane        string
	FirstSkipAt time.Time
	LastSkipAt  time.Time
	SkipCount   int64
}

type doctorCeilingHistory struct {
	Attempts          int
	CeilingDeaths     int
	MaxObservedTokens int64
}

func doctorRuntimeStorePath(configPath string) string {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), "detent.db")
}

func checkDoctorWorkflowLint(ctx context.Context, projectID string, project globalconfig.Project, cfg workflowconfig.Config, storePath string, deps doctorDeps) []doctorCheck {
	workflowPath := doctorWorkflowLintPath(project)
	checks := make([]doctorCheck, 0)
	if len(cfg.ConfiguredSubsettings("budget")) == 0 {
		if budgetCheck, ok := checkDoctorDisabledBudgetCaps(projectID, cfg.Budget); ok {
			budgetCheck.Detail = workflowPath + " " + budgetCheck.Detail
			budgetCheck.Hint = fmt.Sprintf("Set budget.enabled: true in %s, or remove the configured budget caps.", workflowPath)
			checks = append(checks, budgetCheck)
		}
	}
	checks = append(checks, checkDoctorInertWorkflowBlocks(projectID, workflowPath, cfg)...)
	checks = append(checks, checkDoctorGateCommand(ctx, projectID, workflowPath, project, cfg, deps)...)
	checks = append(checks, checkDoctorShipSkill(projectID, workflowPath, cfg, deps)...)
	checks = append(checks, checkDoctorWorkflowRuntimeLint(ctx, projectID, workflowPath, storePath, cfg, deps)...)
	return checks
}

func doctorWorkflowLintPath(project globalconfig.Project) string {
	if ref := strings.TrimSpace(project.WorkflowRef); ref != "" {
		return ref + ":" + strings.TrimSpace(project.Workflow)
	}
	if path, err := resolveDoctorProjectPath(project, project.Workflow); err == nil {
		return path
	}
	return strings.TrimSpace(project.Workflow)
}

func checkDoctorInertWorkflowBlocks(projectID string, workflowPath string, cfg workflowconfig.Config) []doctorCheck {
	blocks := []struct {
		path    string
		enabled bool
	}{
		{path: "budget", enabled: cfg.Budget.Enabled},
		{path: "agent.budget", enabled: cfg.Agent.Budget.Enabled},
		{path: "gate.validator", enabled: cfg.Gate.Validator.Enabled},
		{path: "agent.lessons", enabled: cfg.Agent.Lessons.Enabled},
		{path: "agent.skills", enabled: cfg.Agent.Skills.Enabled},
		{path: "agent.knowledge", enabled: cfg.Agent.Knowledge.Enabled},
	}
	checks := make([]doctorCheck, 0)
	for _, block := range blocks {
		fields := cfg.ConfiguredSubsettings(block.path)
		if block.enabled || len(fields) == 0 {
			continue
		}
		checks = append(checks, doctorCheck{
			Name:   "Project " + projectID + " workflow lint inert " + block.path,
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s configures %s while %s.enabled=false, so the block is inert", workflowPath, strings.Join(fields, ", "), block.path),
			Hint:   fmt.Sprintf("Set %s.enabled: true in %s, or remove the configured %s sub-settings.", block.path, workflowPath, block.path),
		})
	}
	return checks
}

func checkDoctorGateCommand(ctx context.Context, projectID string, workflowPath string, project globalconfig.Project, cfg workflowconfig.Config, deps doctorDeps) []doctorCheck {
	if gate.NormalizeKind(cfg.Gate.Kind) != gate.KindCommand || deps.resolveCommandInDir == nil {
		return nil
	}
	command := strings.TrimSpace(cfg.Gate.Run)
	fields := doctorWorkflowCommandFields(doctorWorkflowCommandPrefix(command))
	if len(fields) == 0 {
		return []doctorCheck{doctorWorkflowGateWarning(projectID, workflowPath, command, "no executable was found", "Set gate.run to a command available on PATH.")}
	}
	executableIndex := doctorWorkflowExecutableIndex(fields)
	if executableIndex < 0 || executableIndex >= len(fields) {
		return []doctorCheck{doctorWorkflowGateWarning(projectID, workflowPath, command, "no executable was found", "Set gate.run to a command available on PATH.")}
	}
	executable := fields[executableIndex]
	environment := append([]string(nil), fields[:executableIndex]...)
	workdir := projectSourceRoot(project, cfg)
	expandedWorkdir, err := expandDoctorWorkspacePath(workdir)
	if err != nil {
		return []doctorCheck{doctorWorkflowGateWarning(projectID, workflowPath, command, fmt.Sprintf("project workdir %q cannot be resolved: %v", workdir, err), "Set workspace.source_root or the project workdir to the repository containing the gate target.")}
	}
	resolved, err := deps.resolveCommandInDir(ctx, expandedWorkdir, environment, executable)
	if err != nil {
		return []doctorCheck{doctorWorkflowGateWarning(projectID, workflowPath, command, fmt.Sprintf("command -v %q failed in %s: %v", executable, expandedWorkdir, err), "Install the executable, fix its project-relative path, or replace gate.run with an available command.")}
	}
	if !doctorMakeExecutable(executable) || deps.runCommandInDir == nil {
		return nil
	}
	commandArgs := []string{}
	if executableIndex+1 < len(fields) {
		commandArgs = fields[executableIndex+1:]
	}
	args := doctorMakeDryRunArgs(commandArgs)
	if err := deps.runCommandInDir(ctx, expandedWorkdir, environment, resolved, args...); err != nil {
		probe := strings.Join(append([]string{executable}, args...), " ")
		return []doctorCheck{doctorWorkflowGateWarning(projectID, workflowPath, command, fmt.Sprintf("dry-run %q failed in %s: %v", probe, expandedWorkdir, err), "Add or fix the make target in the project workdir, or update gate.run to a passing command.")}
	}
	return nil
}

func doctorWorkflowCommandPrefix(command string) string {
	var prefix strings.Builder
	var quote rune
	escaped := false
	for index, r := range command {
		if escaped {
			prefix.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			prefix.WriteRune(r)
			escaped = true
			continue
		}
		if quote != 0 {
			prefix.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			prefix.WriteRune(r)
			quote = r
			continue
		}
		if r == '$' && index+1 < len(command) && command[index+1] == '(' {
			return strings.TrimSpace(prefix.String())
		}
		if strings.ContainsRune("|&;<>(`\n", r) {
			return strings.TrimSpace(prefix.String())
		}
		prefix.WriteRune(r)
	}
	return strings.TrimSpace(prefix.String())
}

func doctorWorkflowExecutableIndex(fields []string) int {
	for index, field := range fields {
		if doctorWorkflowEnvironmentAssignment(field) {
			continue
		}
		return index
	}
	return -1
}

func doctorWorkflowEnvironmentAssignment(field string) bool {
	name, _, ok := strings.Cut(field, "=")
	if !ok || name == "" {
		return false
	}
	for index, r := range name {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func doctorMakeExecutable(executable string) bool {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(executable, "\\", "/")))
	return base == "make" || base == "make.exe" || base == "gmake" || base == "gmake.exe"
}

func doctorMakeDryRunArgs(fields []string) []string {
	args := []string{"-n"}
	for _, field := range fields {
		switch field {
		case "&&", "||", ";", "|":
			return args
		case "-n", "--just-print", "--dry-run", "--recon":
			continue
		default:
			args = append(args, field)
		}
	}
	return args
}

func doctorWorkflowGateWarning(projectID string, workflowPath string, command string, failure string, fix string) doctorCheck {
	return doctorCheck{
		Name:   "Project " + projectID + " workflow lint gate command",
		Status: doctorWarn,
		Detail: fmt.Sprintf("%s gate.run %q cannot work: %s", workflowPath, command, failure),
		Hint:   fix + " Then rerun detent doctor.",
	}
}

func checkDoctorShipSkill(projectID string, workflowPath string, cfg workflowconfig.Config, deps doctorDeps) []doctorCheck {
	if cfg.Deliverable.Kind != workflowconfig.DeliverablePullRequest || !doctorWorkflowHasCodexBackend(cfg) || deps.shipSkillProbe == nil {
		return nil
	}
	codexHome, err := doctorCodexHome(deps.lookupEnv)
	if err == nil {
		_, err = deps.shipSkillProbe(codexHome)
	}
	if err == nil {
		return nil
	}
	return []doctorCheck{{
		Name:   "Project " + projectID + " workflow lint ship skill",
		Status: doctorWarn,
		Detail: fmt.Sprintf("%s uses deliverable.kind=pull_request with a Codex backend, but $go-workflow:ship does not resolve: %v", workflowPath, err),
		Hint:   "Enable go-workflow@gopher-ai in $CODEX_HOME/config.toml and install a cache version whose .codex-plugin/plugin.json version matches its directory and contains skills/ship/SKILL.md.",
	}}
}

func doctorWorkflowHasCodexBackend(cfg workflowconfig.Config) bool {
	for _, backend := range cfg.AgentBackendConfigs() {
		if strings.EqualFold(strings.TrimSpace(backend.Kind), workflowconfig.AgentBackendCodex) {
			return true
		}
	}
	return false
}

func doctorCodexHome(lookupEnv func(string) string) (string, error) {
	if lookupEnv != nil {
		if root := strings.TrimSpace(lookupEnv("CODEX_HOME")); root != "" {
			return root, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve CODEX_HOME: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

func probeDoctorShipSkill(codexHome string) (doctorShipSkill, error) {
	configPath := filepath.Join(codexHome, "config.toml")
	pattern := filepath.Join(codexHome, "plugins", "cache", "*", "go-workflow", "*")
	candidates, globErr := filepath.Glob(pattern)
	if globErr != nil {
		return doctorShipSkill{}, globErr
	}
	invalid := make([]string, 0)
	for _, candidate := range candidates {
		provider := filepath.Base(filepath.Dir(filepath.Dir(candidate)))
		configured, configErr := doctorCodexPluginEnabled(configPath, "go-workflow", provider)
		manifestPath := filepath.Join(candidate, ".codex-plugin", "plugin.json")
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			invalid = append(invalid, manifestPath+" is missing")
			continue
		}
		var manifest struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			invalid = append(invalid, manifestPath+" is invalid")
			continue
		}
		version := strings.TrimSpace(manifest.Version)
		if strings.TrimSpace(manifest.Name) != "go-workflow" || version == "" || version != filepath.Base(candidate) {
			invalid = append(invalid, fmt.Sprintf("%s version %q does not match cache directory %q", manifestPath, version, filepath.Base(candidate)))
			continue
		}
		skillPath := filepath.Join(candidate, "skills", "ship", "SKILL.md")
		if info, err := os.Stat(skillPath); err != nil || info.IsDir() {
			invalid = append(invalid, skillPath+" is missing")
			continue
		}
		if !configured {
			if configErr != nil {
				invalid = append(invalid, fmt.Sprintf("go-workflow cache version %s exists, but plugin enablement cannot be read: %v", version, configErr))
				continue
			}
			invalid = append(invalid, fmt.Sprintf("go-workflow@%s cache version %s exists, but the plugin is not enabled in %s", provider, version, configPath))
			continue
		}
		return doctorShipSkill{Version: version, Path: skillPath}, nil
	}
	if len(invalid) > 0 {
		return doctorShipSkill{}, errors.New(strings.Join(invalid, "; "))
	}
	return doctorShipSkill{}, fmt.Errorf("go-workflow cache is missing under %s", filepath.Join(codexHome, "plugins", "cache"))
}

func doctorCodexPluginEnabled(configPath string, plugin string, provider string) (enabled bool, resultErr error) {
	file, err := os.Open(configPath)
	if err != nil {
		return false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()

	matchingPlugin := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, `[plugins."`), `"]`)
			matchingPlugin = strings.HasPrefix(line, `[plugins."`) && strings.HasSuffix(line, `"]`) && (section == plugin || section == plugin+"@"+provider)
			continue
		}
		if !matchingPlugin {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "enabled" {
			fields := strings.Fields(value)
			if len(fields) > 0 && strings.EqualFold(fields[0], "true") {
				enabled = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return enabled, nil
}

func checkDoctorWorkflowRuntimeLint(ctx context.Context, projectID string, workflowPath string, storePath string, cfg workflowconfig.Config, deps doctorDeps) []doctorCheck {
	if strings.TrimSpace(storePath) == "" || deps.openSQLiteReadOnly == nil || deps.now == nil {
		return nil
	}
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		return nil
	}

	now := deps.now()
	checks := make([]doctorCheck, 0, 2)
	if gate.NormalizeKind(cfg.Gate.Kind) == gate.KindArtifact {
		incidents, err := doctorWaitStatusIncidents(ctx, db, projectID, cfg, now)
		if err == nil && len(incidents) > 0 {
			items := make([]string, 0, len(incidents))
			for _, incident := range incidents {
				items = append(items, fmt.Sprintf("%s in %s for %s (%d skips)", incident.Issue, incident.Lane, now.Sub(incident.FirstSkipAt).Round(time.Minute), incident.SkipCount))
			}
			checks = append(checks, doctorCheck{
				Name:   "Project " + projectID + " workflow lint wait-status liveness",
				Status: doctorWarn,
				Detail: fmt.Sprintf("%s has active attempt==0 items continuously skipped for artifact_gate_wait_status beyond %s with gate.artifact.wait_statuses=%s: %s", workflowPath, doctorWorkflowWaitStatusLiveness, strings.Join(cfg.Gate.Artifact.WaitStatuses, ","), strings.Join(items, "; ")),
				Hint:   fmt.Sprintf("Remove creation-state values from gate.artifact.wait_statuses in %s, seed a dispatchable status, or move the listed items to a dispatchable status.", workflowPath),
			})
		}
	}
	if cfg.Agent.MaxSessionTokens > 0 {
		history, err := doctorCeilingAttemptHistory(ctx, db, projectID, cfg.Agent.MaxSessionTokens, now)
		if err == nil && history.Attempts >= doctorWorkflowCeilingMinAttempts && cfg.Agent.MaxSessionTokens < history.MaxObservedTokens && float64(history.CeilingDeaths)/float64(history.Attempts) >= doctorWorkflowCeilingShare {
			checks = append(checks, doctorCheck{
				Name:   "Project " + projectID + " workflow lint session ceiling",
				Status: doctorWarn,
				Detail: fmt.Sprintf("%s agent.max_session_tokens=%d is below the observed working-set: %d of %d recent attempts (%.0f%%) terminated token_ceiling_exceeded; max observed tokens=%d; each death re-burns the full ceiling — see #1221", workflowPath, cfg.Agent.MaxSessionTokens, history.CeilingDeaths, history.Attempts, float64(history.CeilingDeaths)*100/float64(history.Attempts), history.MaxObservedTokens),
				Hint:   fmt.Sprintf("Recalibrate agent.max_session_tokens in %s above the successful working-set with retry headroom, or remove the cap until measured; assume every ceiling retry re-burns the full configured cap.", workflowPath),
			})
		}
	}
	if err := db.Close(); err != nil {
		checks = append(checks, doctorCheck{
			Name:   "Project " + projectID + " workflow lint telemetry",
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s workflow lint telemetry at %s could not be closed: %v", workflowPath, storePath, err),
			Hint:   "Rerun detent doctor and check the SQLite runtime store for filesystem or locking errors.",
		})
	}
	return checks
}

func doctorWaitStatusIncidents(ctx context.Context, db doctorTelemetryStore, projectID string, cfg workflowconfig.Config, now time.Time) ([]doctorWaitStatusIncident, error) {
	rows, err := db.QueryContext(ctx, `
WITH project_decisions AS (
  SELECT
    id,
    COALESCE(NULLIF(identifier, ''), NULLIF(issue_id, ''), NULLIF(issue_url, ''), 'unassigned') AS issue_key,
    COALESCE(lane, '') AS lane,
    COALESCE(result, '') AS result,
    COALESCE(reason, '') AS reason,
    COALESCE(attempt_number, 0) AS attempt_number,
    decision_at
  FROM scheduler_decisions
  WHERE project_id = ?
    AND decision_at >= ?
), ranked AS (
  SELECT
    id,
    issue_key,
    lane,
    result,
    reason,
    attempt_number,
    decision_at,
    ROW_NUMBER() OVER (PARTITION BY issue_key ORDER BY id DESC) AS latest_rank,
    MAX(CASE WHEN result != 'skipped' OR reason != 'artifact_gate_wait_status' OR attempt_number != 0 THEN id ELSE 0 END)
      OVER (PARTITION BY issue_key) AS reset_id
  FROM project_decisions
), active_issues AS (
  SELECT issue_key, lane, reset_id
  FROM ranked
  WHERE latest_rank = 1
    AND result = 'skipped'
    AND reason = 'artifact_gate_wait_status'
    AND attempt_number = 0
)
SELECT
  active_issues.issue_key,
  active_issues.lane,
  MIN(ranked.decision_at),
  MAX(ranked.decision_at),
  COUNT(*)
FROM active_issues
JOIN ranked ON ranked.issue_key = active_issues.issue_key
WHERE ranked.id > active_issues.reset_id
  AND ranked.result = 'skipped'
  AND ranked.reason = 'artifact_gate_wait_status'
  AND ranked.attempt_number = 0
GROUP BY active_issues.issue_key, active_issues.lane`, strings.TrimSpace(projectID), now.Add(-doctorWorkflowWaitStatusWindow).UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	freshness := 2 * time.Duration(cfg.Polling.IntervalMS) * time.Millisecond
	if freshness < 5*time.Minute {
		freshness = 5 * time.Minute
	}
	incidents := make([]doctorWaitStatusIncident, 0)
	for rows.Next() {
		var incident doctorWaitStatusIncident
		var firstRaw string
		var lastRaw string
		if err := rows.Scan(&incident.Issue, &incident.Lane, &firstRaw, &lastRaw, &incident.SkipCount); err != nil {
			return nil, err
		}
		incident.FirstSkipAt, err = doctorWorkflowSessionTimestamp(firstRaw)
		if err != nil {
			return nil, err
		}
		incident.LastSkipAt, err = doctorWorkflowSessionTimestamp(lastRaw)
		if err != nil {
			return nil, err
		}
		if now.Sub(incident.FirstSkipAt) <= doctorWorkflowWaitStatusLiveness || now.Sub(incident.LastSkipAt) > freshness || !slices.Contains(cfg.Tracker.ActiveStates, incident.Lane) {
			continue
		}
		incidents = append(incidents, incident)
	}
	return incidents, rows.Err()
}

func doctorCeilingAttemptHistory(ctx context.Context, db doctorTelemetryStore, projectID string, configuredCeiling int64, now time.Time) (doctorCeilingHistory, error) {
	rows, err := db.QueryContext(ctx, `
SELECT COALESCE(error_message, ''), COALESCE(metrics_json, '{}')
FROM work_attempts
WHERE project_id = ?
  AND worker_type = 'agent'
  AND completed_at IS NOT NULL
  AND completed_at >= ?
ORDER BY completed_at DESC, id DESC
LIMIT ?`, strings.TrimSpace(projectID), now.Add(-doctorWorkflowCeilingWindow).UTC().Format(time.RFC3339Nano), doctorWorkflowCeilingSampleLimit)
	if err != nil {
		return doctorCeilingHistory{}, err
	}
	defer rows.Close()

	var history doctorCeilingHistory
	for rows.Next() {
		var errorMessage string
		var metricsJSON string
		if err := rows.Scan(&errorMessage, &metricsJSON); err != nil {
			return doctorCeilingHistory{}, err
		}
		history.Attempts++
		var metrics struct {
			TotalTokens int64 `json:"total_tokens"`
		}
		if err := json.Unmarshal([]byte(metricsJSON), &metrics); err == nil && metrics.TotalTokens > history.MaxObservedTokens {
			history.MaxObservedTokens = metrics.TotalTokens
		}
		if strings.Contains(errorMessage, "session token ceiling exceeded") &&
			strings.Contains(errorMessage, "source=max_session_tokens") &&
			doctorWorkflowErrorInt64(errorMessage, "ceiling_tokens") == configuredCeiling {
			history.CeilingDeaths++
			if total := doctorWorkflowErrorInt64(errorMessage, "total_tokens"); total > history.MaxObservedTokens {
				history.MaxObservedTokens = total
			}
		}
	}
	return history, rows.Err()
}
