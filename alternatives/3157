package tracker

import "time"

const NativeGitHubAggregateLimit = 128

type NativeGitHubKey struct {
	Stage          string `json:"stage"`
	Step           string `json:"step"`
	EndpointFamily string `json:"endpoint_family"`
	Outcome        string `json:"outcome"`
}

type NativeGitHubCount struct {
	NativeGitHubKey
	Count int64 `json:"count"`
}

type NativeGitHubTiming struct {
	NativeGitHubKey
	Protocol        string    `json:"protocol"`
	QueryPurpose    string    `json:"query_purpose"`
	Boundary        string    `json:"boundary"`
	AttemptCount    int64     `json:"attempt_count"`
	TimedCount      int64     `json:"timed_count"`
	ElapsedSumNS    *int64    `json:"elapsed_sum_ns"`
	ElapsedMaxNS    *int64    `json:"elapsed_max_ns"`
	FirstObservedAt time.Time `json:"first_observed_at"`
	LastObservedAt  time.Time `json:"last_observed_at"`
}

type NativeGitHubScope struct {
	Scope            string               `json:"scope"`
	StartedAt        time.Time            `json:"started_at"`
	ObservedAt       time.Time            `json:"observed_at"`
	WallElapsedNS    *int64               `json:"wall_elapsed_ns"`
	SubStepElapsedNS *int64               `json:"sub_step_elapsed_ns"`
	RESTCounts       []NativeGitHubCount  `json:"rest_counts"`
	Timings          []NativeGitHubTiming `json:"timings"`
	CountsDropped    int                  `json:"counts_dropped"`
	TimingsDropped   int                  `json:"timings_dropped"`
	Coverage         string               `json:"coverage"`
	ElapsedSemantics string               `json:"elapsed_semantics"`
	HTTPBoundary     string               `json:"http_boundary"`
	TimingUnit       string               `json:"timing_unit"`
	BuildCoverage    string               `json:"build_coverage"`
}

type NativeGitHubTimingEvidence struct {
	ProjectID   ProjectID             `json:"project_id"`
	WorkItemID  NativeWorkItemID      `json:"work_item_id"`
	AttemptID   string                `json:"native_attempt_id"`
	RunnerID    string                `json:"runner_id"`
	Landing     *NativeLandingReceipt `json:"landing"`
	Scope       *NativeGitHubScope    `json:"observation"`
	Unavailable []string              `json:"unavailable"`
}

func (e NativeRuntimeEvidence) GitHubTimings(attempt, runner string) (NativeGitHubTimingEvidence, bool) {
	if e.Attempt == nil || attempt == "" || runner == "" || e.Attempt.AttemptID != attempt || e.Attempt.RunnerID != runner {
		return NativeGitHubTimingEvidence{}, false
	}
	result := NativeGitHubTimingEvidence{ProjectID: e.Issue.ProjectID, WorkItemID: e.Issue.WorkItemID, AttemptID: attempt, RunnerID: runner, Unavailable: []string{}}
	if e.Attempt.Runtime != nil {
		result.Scope, result.Landing = e.Attempt.Runtime.GitHub, e.Attempt.Runtime.Landing
	}
	if result.Scope == nil {
		result.Unavailable = append(result.Unavailable, "github_scope_timings")
	} else {
		for _, boundary := range []struct{ name, protocol, boundary, family string }{
			{"rest_http", "rest", "http_transport", ""},
			{"graphql_http", "graphql", "http_transport", ""},
			{"rest_token_inclusive", "rest", "token_resolution_inclusive", ""},
			{"graphql_token_inclusive", "graphql", "token_resolution_inclusive", ""},
			{"installation_http", "rest", "http_transport", "app installation tokens"},
		} {
			observed := false
			for _, timing := range result.Scope.Timings {
				if timing.Protocol == boundary.protocol && timing.Boundary == boundary.boundary && (boundary.family == "" || timing.EndpointFamily == boundary.family) && timing.TimedCount > 0 {
					observed = true
				}
			}
			if !observed {
				result.Unavailable = append(result.Unavailable, boundary.name)
			}
		}
		if result.Scope.WallElapsedNS == nil {
			result.Unavailable = append(result.Unavailable, "scope_wall")
		}
		if result.Scope.SubStepElapsedNS == nil {
			result.Unavailable = append(result.Unavailable, "sub_step_wall")
		}
		if result.Scope.BuildCoverage == "unavailable" {
			result.Unavailable = append(result.Unavailable, "observed_build")
		}
	}
	if result.Landing == nil {
		result.Unavailable = append(result.Unavailable, "attempt_landing_receipt")
	}
	return result, true
}
