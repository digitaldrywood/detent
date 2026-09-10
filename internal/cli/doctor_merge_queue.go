package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func defaultDoctorGitHubBranchMergePolicy(ctx context.Context, cfg workflowconfig.Config, repository string) (ghconnector.BranchMergePolicy, error) {
	client, err := ghconnector.NewConnector(doctorGitHubConnectorConfig(cfg))
	if err != nil {
		return ghconnector.BranchMergePolicy{}, err
	}
	return client.RepositoryStrictMergePolicy(ctx, repository)
}

func checkDoctorMergeQueue(ctx context.Context, id string, project globalconfig.Project, cfg workflowconfig.Config, storePath string, deps doctorDeps) (check doctorCheck) {
	check = doctorCheck{Name: "Project " + id + " merge queue recommendation", Status: doctorOK, Detail: "no measured strict-protection merge pressure"}
	if strings.TrimSpace(storePath) == "" {
		check.Detail = "no merge history recorded yet"
		return check
	}
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		check.Detail = "merge history unavailable: " + err.Error()
		return check
	}
	defer func() {
		if err := db.Close(); err != nil {
			check.Status = doctorWarn
			check.Detail += "; close merge history: " + err.Error()
		}
	}()
	for _, repository := range doctorGitHubRepositories(ctx, project, cfg, deps, projectSourceRoot(project, cfg)) {
		policy, err := deps.githubBranchPolicy(ctx, cfg, repository)
		if err != nil {
			check.Status = doctorWarn
			check.Detail = "branch protection could not be read: " + err.Error()
			return check
		}
		if policy.RulesUnavailableOnPlan {
			check.Detail = repository + ": merge queue not available on this plan"
			continue
		}
		if !policy.Strict || policy.MergeQueue {
			continue
		}
		detail, err := doctorMergeQueueHistory(ctx, db, id, repository, policy.Branch, deps.now())
		if err != nil {
			check.Status = doctorWarn
			check.Detail = "merge history could not be read: " + err.Error()
			return check
		}
		if detail != "" {
			check.Status = doctorWarn
			check.Detail = detail
			check.Hint = "Consider enabling GitHub's merge queue for this branch and running required checks on merge_group; detent doctor does not change repository settings."
			return check
		}
	}
	return check
}

func doctorMergeQueueHistory(ctx context.Context, db doctorTelemetryStore, projectID, repository, branch string, now time.Time) (string, error) {
	const window = 7 * 24 * time.Hour
	rows, err := db.QueryContext(ctx, `SELECT started_at, metadata_json FROM workflow_phase_events
WHERE project_id = ? AND phase_type = 'lane' AND status = 'entered' AND reason = 'pull_request_merged'
AND julianday(started_at) >= julianday(?) AND julianday(started_at) <= julianday(?)
ORDER BY julianday(started_at), id`, projectID, now.Add(-window).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	seen := map[int64]bool{}
	durations := []int64{}
	var first, last time.Time
	for rows.Next() {
		var rawAt, rawMetadata string
		if err := rows.Scan(&rawAt, &rawMetadata); err != nil {
			return "", err
		}
		var metadata struct {
			PullRequest struct {
				Repository        string `json:"repository"`
				BaseRef           string `json:"base_ref"`
				Number            int64  `json:"number"`
				CIDurationSeconds int64  `json:"ci_duration_seconds"`
			} `json:"pull_request"`
		}
		if err := json.Unmarshal([]byte(rawMetadata), &metadata); err != nil {
			return "", err
		}
		pr := metadata.PullRequest
		if !strings.EqualFold(pr.Repository, repository) || pr.BaseRef != branch || pr.Number <= 0 || seen[pr.Number] {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, rawAt)
		if err != nil {
			return "", err
		}
		seen[pr.Number] = true
		if first.IsZero() {
			first = at
		}
		last = at
		if pr.CIDurationSeconds > 0 {
			durations = append(durations, pr.CIDurationSeconds)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(seen) < 3 || len(durations) < 3 || !last.After(first) {
		return "", nil
	}
	slices.Sort(durations)
	median := float64(durations[len(durations)/2])
	if len(durations)%2 == 0 {
		median = (median + float64(durations[len(durations)/2-1])) / 2
	}
	cadence := float64(len(seen)-1) / last.Sub(first).Hours()
	overlap := cadence * median / 3600
	if overlap < 0.25 {
		return "", nil
	}
	return fmt.Sprintf("%s branch %s requires up-to-date checks; %d distinct recorded merges over %.1fh (%.1f/day), median CI %.1f minutes from %d samples; %.2f merges per CI duration indicates likely head invalidation", repository, branch, len(seen), last.Sub(first).Hours(), cadence*24, median/60, len(durations), overlap), nil
}
