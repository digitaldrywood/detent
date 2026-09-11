package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

const doctorInvariantWindow = 7 * 24 * time.Hour

// checkDoctorInvariantEvidence reports runtime evidence against docs/invariants.md.
// Each check is named by its invariant ID so a failing fleet points at the
// contract it broke; internal/invariants enforces the source-level half in CI.
func checkDoctorInvariantEvidence(ctx context.Context, id string, project globalconfig.Project, cfg workflowconfig.Config, storePath string, deps doctorDeps) []doctorCheck {
	deps = deps.withDefaults()
	since := deps.now().Add(-doctorInvariantWindow).UTC().Format(time.RFC3339Nano)
	checks := []doctorCheck{doctorInvariantCheck(id, "INV-3", "no new mechanisms", doctorOK, "enforced by internal/invariants tests in CI; no runtime evidence required")}

	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		checks = append(checks, doctorInvariantCheck(id, "INV-1", "single lane writer", doctorWarn, "runtime store unavailable: "+err.Error()))
		return append(checks, doctorInvariantRepositoryChecks(ctx, id, project, cfg, deps, nil, since)...)
	}
	defer func() { _ = db.Close() }()

	var transitions, writes int64
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM workflow_phase_events WHERE project_id = ? AND started_at > ? AND previous_phase_name IS NOT NULL AND previous_phase_name <> ''), (SELECT count(*) FROM lane_ledger WHERE project_id = ? AND written_at > ?)`, id, since, id, since).Scan(&transitions, &writes); err != nil {
		checks = append(checks, doctorInvariantCheck(id, "INV-1", "single lane writer", doctorWarn, "evidence query failed: "+err.Error()))
	} else if transitions > 0 && writes == 0 {
		checks = append(checks, doctorInvariantCheck(id, "INV-1", "single lane writer", doctorFail, fmt.Sprintf("%d lane transitions in 7d with no lane-ledger writes", transitions)))
	} else {
		checks = append(checks, doctorInvariantCheck(id, "INV-1", "single lane writer", doctorOK, fmt.Sprintf("%d lane transitions, %d ledger writes in 7d", transitions, writes)))
	}

	var infraParks int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM work_attempts a JOIN workflow_phase_events e ON e.project_id = a.project_id AND e.identifier = a.identifier AND e.phase_name = 'Blocked' AND julianday(e.started_at) BETWEEN julianday(a.completed_at) AND julianday(a.completed_at) + 10.0/1440 WHERE a.project_id = ? AND a.completed_at > ? AND a.error_class IN ('workspace', 'backend_startup_timeout', 'backend_startup_failure', 'runner_error')`, id, since).Scan(&infraParks); err != nil {
		checks = append(checks, doctorInvariantCheck(id, "INV-2", "instance attribution", doctorWarn, "evidence query failed: "+err.Error()))
	} else if infraParks > 0 {
		checks = append(checks, doctorInvariantCheck(id, "INV-2", "instance attribution", doctorFail, fmt.Sprintf("%d pre-turn infrastructure failure(s) were followed by a Blocked park of the issue within 10 minutes in 7d", infraParks)))
	} else {
		checks = append(checks, doctorInvariantCheck(id, "INV-2", "instance attribution", doctorOK, "no issue was parked for a pre-turn infrastructure failure in 7d"))
	}

	var retired int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_phase_events WHERE project_id = ? AND started_at > ? AND (reason LIKE 'worker_lane_revocation%' OR reason LIKE '%indeterminate lane%' OR reason = 'workspace_preparation_retry_limit')`, id, since).Scan(&retired); err != nil {
		checks = append(checks, doctorInvariantCheck(id, "INV-9", "retired mechanisms", doctorWarn, "evidence query failed: "+err.Error()))
	} else if retired > 0 {
		checks = append(checks, doctorInvariantCheck(id, "INV-9", "retired mechanisms", doctorFail, fmt.Sprintf("%d lane event(s) in 7d carry a retired reason (lane revocation, indeterminate lane stop, or per-issue workspace retry limit)", retired)))
	} else {
		checks = append(checks, doctorInvariantCheck(id, "INV-9", "retired mechanisms", doctorOK, "no retired reason codes recorded in 7d"))
	}

	return append(checks, doctorInvariantRepositoryChecks(ctx, id, project, cfg, deps, db, since)...)
}

func doctorInvariantCheck(id, inv, short string, status doctorStatus, detail string) doctorCheck {
	check := doctorCheck{Name: "Project " + id + " " + inv + " " + short, Status: status, Detail: detail}
	if status != doctorOK {
		check.Hint = "See docs/invariants.md " + inv + "; fix the mechanism, never add a guard."
	}
	return check
}

func doctorInvariantRepositoryChecks(ctx context.Context, id string, project globalconfig.Project, cfg workflowconfig.Config, deps doctorDeps, db doctorTelemetryStore, since string) []doctorCheck {
	var checks []doctorCheck
	if doctorTrackerUsesGitHubReads(cfg.Tracker.Kind) && cfg.Deliverable.Kind == workflowconfig.DeliverablePullRequest {
		for _, repository := range doctorGitHubRepositories(ctx, project, cfg, deps, projectSourceRoot(project, cfg)) {
			policy, err := deps.githubBranchPolicy(ctx, cfg, repository)
			switch {
			case err != nil:
				checks = append(checks, doctorInvariantCheck(id, "INV-8", "no strict protection", doctorWarn, repository+": branch policy could not be read: "+err.Error()))
			case policy.RulesUnavailableOnPlan:
				checks = append(checks, doctorInvariantCheck(id, "INV-8", "no strict protection", doctorOK, repository+": branch rules are not available on this plan; nothing to enforce"))
			default:
				if policy.Strict {
					check := doctorInvariantCheck(id, "INV-8", "no strict protection", doctorFail, repository+" branch "+policy.Branch+" requires branches to be up to date before merging; every merge invalidates every open PR")
					check.Hint = "Disable strict status checks on " + repository + " and use a merge queue (docs/invariants.md INV-8)."
					checks = append(checks, check)
				} else {
					checks = append(checks, doctorInvariantCheck(id, "INV-8", "no strict protection", doctorOK, repository+" branch "+policy.Branch+" does not require up-to-date branches"))
				}
				checks = append(checks, doctorInvariantQueueCheck(ctx, id, repository, policy.Branch, policy.MergeQueue, db, since))
			}
		}
	}
	return append(checks, doctorInvariantWorkflowCheck(id, project))
}

func doctorInvariantQueueCheck(ctx context.Context, id, repository, branch string, queue bool, db doctorTelemetryStore, since string) doctorCheck {
	if !queue {
		return doctorInvariantCheck(id, "INV-4", "queue is the merge path", doctorOK, repository+": no merge queue on "+branch+"; the serialized merge worker applies")
	}
	if db == nil {
		return doctorInvariantCheck(id, "INV-4", "queue is the merge path", doctorWarn, repository+": merge queue present on "+branch+" but the runtime store is unavailable")
	}
	var programmatic int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM work_attempts WHERE project_id = ? AND completed_at > ? AND error_class = 'programmatic_merge_failed'`, id, since).Scan(&programmatic); err != nil {
		return doctorInvariantCheck(id, "INV-4", "queue is the merge path", doctorWarn, "evidence query failed: "+err.Error())
	}
	if programmatic > 0 {
		check := doctorInvariantCheck(id, "INV-4", "queue is the merge path", doctorFail, fmt.Sprintf("%s: merge queue present on %s but %d programmatic merge attempt(s) failed in 7d", repository, branch, programmatic))
		check.Hint = "Merges must go through the queue (docs/invariants.md INV-4)."
		return check
	}
	return doctorInvariantCheck(id, "INV-4", "queue is the merge path", doctorOK, repository+": merge queue present on "+branch+"; no programmatic merge attempts in 7d")
}

// doctorInvariantWorkflowCheck reports whether the project's own CI runs real
// jobs on pull_request events. Projects may choose that, so it warns.
func doctorInvariantWorkflowCheck(id string, project globalconfig.Project) doctorCheck {
	check := doctorInvariantCheck(id, "INV-5", "CI once per ready head", doctorOK, "no .github/workflows/ci.yml in the project checkout; nothing to measure")
	workdir := strings.TrimSpace(project.Workdir)
	if workdir == "" {
		return check
	}
	data, err := os.ReadFile(filepath.Join(expandDoctorHomePath(workdir), ".github", "workflows", "ci.yml"))
	if err != nil {
		return check
	}
	return doctorInvariantWorkflowVerdict(id, data)
}

func doctorInvariantWorkflowVerdict(id string, data []byte) doctorCheck {
	var workflow struct {
		On   map[string]any `yaml:"on"`
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return doctorInvariantCheck(id, "INV-5", "CI once per ready head", doctorWarn, "ci.yml could not be parsed: "+err.Error())
	}
	if _, ok := workflow.On["pull_request"]; !ok {
		return doctorInvariantCheck(id, "INV-5", "CI once per ready head", doctorOK, "ci.yml has no pull_request trigger; CI runs only in the merge queue and on main")
	}
	var realOnPR []string
	for job, spec := range workflow.Jobs {
		condition := strings.TrimSpace(spec.If)
		if strings.Contains(condition, "github.event_name != 'pull_request'") || condition == "github.event_name == 'pull_request'" {
			continue
		}
		realOnPR = append(realOnPR, job)
	}
	if len(realOnPR) == 0 {
		return doctorInvariantCheck(id, "INV-5", "CI once per ready head", doctorOK, "pull_request events run only placeholder checks; real CI runs in the merge queue and on main")
	}
	check := doctorInvariantCheck(id, "INV-5", "CI once per ready head", doctorWarn, fmt.Sprintf("%d job(s) run on every pull_request push (%s); the default is one run per ready head, but this is the project's choice", len(realOnPR), strings.Join(sortedStrings(realOnPR), ", ")))
	check.Hint = "docs/invariants.md INV-5: guard jobs with github.event_name != 'pull_request' or gate CI with gate.ci_trigger_label."
	return check
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func expandDoctorHomePath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
