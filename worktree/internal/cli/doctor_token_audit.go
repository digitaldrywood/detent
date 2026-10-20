package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func checkDoctorTokenAccounting(ctx context.Context, id, storePath string, deps doctorDeps) (check doctorCheck) {
	check = doctorCheck{Name: "Project " + id + " token_accounting", Status: doctorWarn}
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		check.Detail = "runtime store unavailable: " + err.Error()
		return check
	}
	defer func() {
		if err := db.Close(); err != nil {
			check.Status = doctorWarn
			check.Detail += "; close store: " + err.Error()
		}
	}()
	now := deps.now().UTC()
	since, until := now.Add(-24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)
	var usage, attempts int64
	err = db.QueryRowContext(ctx, `SELECT
 (SELECT COALESCE(sum(total_tokens), 0) FROM usage_events WHERE project_id = ? AND julianday(finished_at) > julianday(?) AND julianday(finished_at) <= julianday(?)),
 (SELECT COALESCE(sum(json_extract(metrics_json, '$.total_tokens')), 0) FROM work_attempts WHERE project_id = ? AND julianday(COALESCE(completed_at, started_at)) > julianday(?) AND julianday(COALESCE(completed_at, started_at)) <= julianday(?))`, id, since, until, id, since, until).Scan(&usage, &attempts)
	if err != nil {
		check.Detail = "token evidence query failed: " + err.Error()
		return check
	}
	check.Status = doctorOK
	difference := math.Abs(float64(usage) - float64(attempts))
	percent := 0.0
	if usage > 0 {
		percent = 100 * difference / float64(usage)
	} else if attempts != 0 {
		percent = 100
	}
	if percent > 5 {
		check.Status = doctorWarn
	}
	check.Detail = fmt.Sprintf("last 24h: usage_events=%d tokens; work_attempts.metrics_json=%d tokens; difference %.2f%% of usage_events", usage, attempts, percent)
	check.Hint = "Use usage_events for reconciled token reports. Completion-time windows can differ for sessions crossing the window boundary."
	return check
}

func checkDoctorModelPolicy(ctx context.Context, id, storePath string, cfg workflowconfig.Config, deps doctorDeps) (check doctorCheck) {
	check = doctorCheck{Name: "Project " + id + " model_policy", Status: doctorWarn}
	policy := cfg.EffectiveModelSelection()
	var levels []string
	for level := range policy.Levels {
		levels = append(levels, level)
	}
	sort.Strings(levels)
	var details []string
	for _, name := range levels {
		level := policy.Levels[name]
		alias := doctorPolicyString(level.Model)
		model := policy.Model(alias)
		modelSource := doctorPolicySource(policy, "levels."+name+".model")
		if alias == "normal" || alias == "complex" {
			modelSource += "; " + alias + "_model=" + doctorPolicySource(policy, alias+"_model")
		}
		details = append(details, fmt.Sprintf("level %s: model=%s (source=%s), effort=%s (source=%s)", name, model, modelSource, doctorPolicyString(level.Effort), doctorPolicySource(policy, "levels."+name+".effort")))
	}
	if !policy.Active() {
		details = append(details, "automatic selection disabled; backend/route defaults apply")
	}
	if len(levels) == 0 {
		details = append(details, "no model-selection levels configured")
	}
	check.Detail = strings.Join(details, "; ")
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		check.Detail += "; runtime store unavailable: " + err.Error()
		return check
	}
	defer func() {
		if err := db.Close(); err != nil {
			check.Status = doctorWarn
			check.Detail += "; close store: " + err.Error()
		}
	}()
	now := deps.now().UTC()
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(runtime_identity_json, '{}'), COALESCE(reasoning_effort, ''), COALESCE(reasoning_effort_provenance, '') FROM codex_sessions WHERE project_id = ? AND julianday(started_at) > julianday(?) AND julianday(started_at) <= julianday(?)`, id, now.Add(-24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		check.Detail += "; session evidence query failed: " + err.Error()
		return check
	}
	defer rows.Close()
	var total, above, unknown int
	for rows.Next() {
		var raw, effort, source string
		if err := rows.Scan(&raw, &effort, &source); err != nil {
			check.Detail += "; read session: " + err.Error()
			return check
		}
		total++
		var identity agentidentity.Identity
		if err := json.Unmarshal([]byte(raw), &identity); err != nil {
			unknown++
			continue
		}
		if identity.Selection.EffortSource != "" {
			source = identity.Selection.EffortSource
		}
		if identity.ReasoningEffort.Value != "" {
			effort = identity.ReasoningEffort.Value
		}
		level, found := policy.Levels[identity.Selection.Level]
		if source == "" || !found || doctorEffortRank(effort) < 0 || doctorEffortRank(doctorPolicyString(level.Effort)) < 0 {
			unknown++
			continue
		}
		if (strings.HasPrefix(source, "issue.") && strings.HasSuffix(source, ".effort")) && doctorEffortRank(effort) > doctorEffortRank(doctorPolicyString(level.Effort)) {
			above++
		}
	}
	if err := rows.Err(); err != nil {
		check.Detail += "; read sessions: " + err.Error()
		return check
	}
	check.Status = doctorOK
	if above*10 > total || unknown > 0 {
		check.Status = doctorWarn
	}
	check.Detail += fmt.Sprintf("; last 24h: %d/%d dispatches used issue.effort (including role overrides) above the current level default; %d lack comparable provenance", above, total, unknown)
	check.Hint = "Compare printed policy sources with the operator's intended fleet policy; warn threshold is more than 10% of dispatches. Historical comparisons use current level defaults."
	return check
}

func doctorPolicyString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func doctorPolicySource(policy workflowconfig.ModelSelection, key string) string {
	if source := policy.Sources[key]; source != "" {
		return source
	}
	return "unspecified"
}

func doctorEffortRank(effort string) int {
	switch effort {
	case "none":
		return 0
	case "minimal":
		return 1
	case "low":
		return 2
	case "medium":
		return 3
	case "high":
		return 4
	case "xhigh":
		return 5
	case "max":
		return 6
	default:
		return -1
	}
}
