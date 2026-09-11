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
	check = doctorCheck{Name: "Project " + id + " merge queue recommendation", Status: doctorOK, Detail: "no measured merge pressure"}
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
	var recommendations []string
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
		history, err := doctorMergeQueueHistory(ctx, db, id, repository, policy.Branch, deps.now())
		if err != nil {
			check.Status = doctorWarn
			check.Detail = "merge history could not be read: " + err.Error()
			return check
		}
		runs, err := doctorPullRequestCIRunHistory(ctx, db, id, repository, deps.now())
		if err != nil {
			check.Status = doctorWarn
			check.Detail = "pull request history could not be read: " + err.Error()
			return check
		}
		recommendations = append(recommendations, doctorMergeRecommendations(repository, policy, history, runs)...)
	}
	if len(recommendations) == 0 {
		return check
	}
	check.Status = doctorWarn
	check.Detail = strings.Join(recommendations, "; ")
	check.Hint = "Recommendations cite recorded history; detent doctor never changes repository or queue settings."
	return check
}

type doctorMergeHistory struct {
	merges          int
	hours           float64
	mergesPerHour   float64
	medianCISeconds float64
	ciSamples       int
}

// mergesPerCI is how many merges land during one median CI run; above one,
// CI cannot keep up with the merge rate without batching.
func (h doctorMergeHistory) mergesPerCI() float64 {
	return h.mergesPerHour * h.medianCISeconds / 3600
}

func (h doctorMergeHistory) measured() bool {
	return h.merges >= 3 && h.ciSamples >= 3 && h.hours > 0
}

type doctorPullRequestCIRuns struct {
	pullRequests int
	heads        int
}

func (r doctorPullRequestCIRuns) runsPerPullRequest() float64 {
	if r.pullRequests == 0 {
		return 0
	}
	return float64(r.heads) / float64(r.pullRequests)
}

func doctorMergeRecommendations(repository string, policy ghconnector.BranchMergePolicy, history doctorMergeHistory, runs doctorPullRequestCIRuns) []string {
	var out []string
	if history.measured() {
		overlap := history.mergesPerCI()
		switch {
		case policy.Strict && !policy.MergeQueue && overlap >= 0.25:
			out = append(out, fmt.Sprintf("queue: %s branch %s requires up-to-date checks; %d distinct recorded merges over %.1fh (%.1f/day), median CI %.1f minutes from %d samples; %.2f merges per CI duration indicates likely head invalidation; enable GitHub's merge queue and run required checks on merge_group", repository, policy.Branch, history.merges, history.hours, history.mergesPerHour*24, history.medianCISeconds/60, history.ciSamples, overlap))
		case policy.MergeQueue && overlap > 1:
			out = append(out, fmt.Sprintf("batching: %s branch %s merges %.2f per hour with median CI %.1f minutes (%.2f merges per CI duration > 1); raise the queue's minimum entries to merge or its wait time so one CI run covers several PRs", repository, policy.Branch, history.mergesPerHour, history.medianCISeconds/60, overlap))
		}
	}
	if runs.pullRequests >= 3 && runs.runsPerPullRequest() > 1.5 {
		out = append(out, fmt.Sprintf("draft convention: %s pull requests pushed %d distinct heads across %d PRs in 7d (%.2f CI runs per PR > 1.5); open PRs as drafts and mark ready once so CI runs once per ready head", repository, runs.heads, runs.pullRequests, runs.runsPerPullRequest()))
	}
	return out
}

func doctorMergeQueueHistory(ctx context.Context, db doctorTelemetryStore, projectID, repository, branch string, now time.Time) (doctorMergeHistory, error) {
	const window = 7 * 24 * time.Hour
	rows, err := db.QueryContext(ctx, `SELECT started_at, metadata_json FROM workflow_phase_events
WHERE project_id = ? AND phase_type = 'lane' AND status = 'entered' AND reason = 'pull_request_merged'
AND julianday(started_at) >= julianday(?) AND julianday(started_at) <= julianday(?)
ORDER BY julianday(started_at), id`, projectID, now.Add(-window).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return doctorMergeHistory{}, err
	}
	defer rows.Close()
	seen := map[int64]bool{}
	durations := []int64{}
	var first, last time.Time
	for rows.Next() {
		var rawAt, rawMetadata string
		if err := rows.Scan(&rawAt, &rawMetadata); err != nil {
			return doctorMergeHistory{}, err
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
			return doctorMergeHistory{}, err
		}
		pr := metadata.PullRequest
		if !strings.EqualFold(pr.Repository, repository) || pr.BaseRef != branch || pr.Number <= 0 || seen[pr.Number] {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, rawAt)
		if err != nil {
			return doctorMergeHistory{}, err
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
		return doctorMergeHistory{}, err
	}
	history := doctorMergeHistory{merges: len(seen), ciSamples: len(durations)}
	if len(seen) < 2 || !last.After(first) {
		return history, nil
	}
	history.hours = last.Sub(first).Hours()
	history.mergesPerHour = float64(len(seen)-1) / history.hours
	if len(durations) > 0 {
		slices.Sort(durations)
		history.medianCISeconds = float64(durations[len(durations)/2])
		if len(durations)%2 == 0 {
			history.medianCISeconds = (history.medianCISeconds + float64(durations[len(durations)/2-1])) / 2
		}
	}
	return history, nil
}

// Every distinct recorded PR head is one CI run under per-push CI.
func doctorPullRequestCIRunHistory(ctx context.Context, db doctorTelemetryStore, projectID, repository string, now time.Time) (doctorPullRequestCIRuns, error) {
	const window = 7 * 24 * time.Hour
	rows, err := db.QueryContext(ctx, `SELECT metadata_json FROM workflow_phase_events
WHERE project_id = ? AND phase_type = 'lane' AND julianday(started_at) >= julianday(?) AND julianday(started_at) <= julianday(?)
AND metadata_json LIKE '%"head_sha"%'`, projectID, now.Add(-window).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return doctorPullRequestCIRuns{}, err
	}
	defer rows.Close()
	heads := map[int64]map[string]bool{}
	for rows.Next() {
		var rawMetadata string
		if err := rows.Scan(&rawMetadata); err != nil {
			return doctorPullRequestCIRuns{}, err
		}
		var metadata struct {
			PullRequest struct {
				Repository string `json:"repository"`
				Number     int64  `json:"number"`
				HeadSHA    string `json:"head_sha"`
			} `json:"pull_request"`
		}
		if err := json.Unmarshal([]byte(rawMetadata), &metadata); err != nil {
			return doctorPullRequestCIRuns{}, err
		}
		pr := metadata.PullRequest
		if !strings.EqualFold(pr.Repository, repository) || pr.Number <= 0 || strings.TrimSpace(pr.HeadSHA) == "" {
			continue
		}
		if heads[pr.Number] == nil {
			heads[pr.Number] = map[string]bool{}
		}
		heads[pr.Number][strings.TrimSpace(pr.HeadSHA)] = true
	}
	if err := rows.Err(); err != nil {
		return doctorPullRequestCIRuns{}, err
	}
	runs := doctorPullRequestCIRuns{pullRequests: len(heads)}
	for _, set := range heads {
		runs.heads += len(set)
	}
	return runs, nil
}
