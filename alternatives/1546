package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/skills"
)

func updateRunnerSkills(ctx context.Context, tx *sql.Tx, scope nativeScope, report *[]skills.ProviderSkill, now time.Time) error {
	if report == nil {
		return nil
	}
	if scope.credential.Runner.RunnerID == "" {
		return nativeInvalid("Skill reports require an enrolled runner")
	}
	for _, skill := range *report {
		if !skills.ValidProviderSkill(skill) {
			return nativeInvalid("Invalid runner skill metadata")
		}
	}
	raw, err := json.Marshal(*report)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runner_project_skills (runner_id, project_id, skills_json, reported_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (runner_id, project_id) DO UPDATE SET skills_json = excluded.skills_json, reported_at = excluded.reported_at`, scope.credential.Runner.RunnerID, scope.project, string(raw), formatHubTime(now))
	return err
}

func readConversationSkills(ctx context.Context, tx *sql.Tx, record conversationRecord) ([]skills.ProviderSkill, error) {
	rows, err := tx.QueryContext(ctx, `SELECT s.skills_json FROM runner_project_skills s
		JOIN runner_identities r ON r.id = s.runner_id
		JOIN api_tokens t ON t.id = r.token_id
		JOIN token_grants g ON g.token_id = r.token_id AND g.project_id = s.project_id AND g.organization_id = r.organization_id
		WHERE s.project_id = ? AND r.organization_id = ? AND r.state != 'disabled' AND t.revoked_at IS NULL
		AND (? = '' OR s.runner_id = ?)
		ORDER BY s.reported_at DESC, s.runner_id`, record.ProjectID, record.OrganizationID, record.Execution.Owner.RunnerID, record.Execution.Owner.RunnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]skills.ProviderSkill, 0)
	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var report []skills.ProviderSkill
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			return nil, err
		}
		for _, skill := range report {
			name := strings.ToLower(skill.Name)
			if seen[name] {
				continue
			}
			seen[name] = true
			result = append(result, skill)
		}
	}
	return result, rows.Err()
}
