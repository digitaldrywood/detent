package hubserver

import (
	"net/http"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) getNativeGitHubTimings(c echo.Context) error {
	for key, values := range c.QueryParams() {
		if key != "native_attempt_id" && key != "runner_id" || len(values) != 1 {
			return s.nativeAPIError(c, nativeInvalid("Unsupported GitHub timing selector"))
		}
	}
	attempt, runner := c.QueryParam("native_attempt_id"), c.QueryParam("runner_id")
	if !validNativeID(attempt, "attempt") || !validNativeID(runner, "runner") {
		return s.nativeAPIError(c, nativeInvalid("Select a native attempt and registered runner"))
	}
	scope := nativeRequestScope(c)
	if scope.credential.Runner.RunnerID != "" && scope.credential.Runner.RunnerID != runner {
		return s.nativeAPIError(c, nativeNotFound())
	}
	evidence, err := s.readNativeRuntime(c.Request().Context(), scope, c.Param("item"), attempt)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	data, ok := evidence.GitHubTimings(attempt, runner)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	return c.JSON(http.StatusOK, data)
}

func validateNativeGitHubScope(scope *tracker.NativeGitHubScope) error {
	if scope == nil {
		return nil
	}
	if scope.Scope != "native_landing" || scope.StartedAt.IsZero() || scope.ObservedAt.Before(scope.StartedAt) || len(scope.RESTCounts) > tracker.NativeGitHubAggregateLimit || len(scope.Timings) > tracker.NativeGitHubAggregateLimit || scope.CountsDropped < 0 || scope.TimingsDropped < 0 || scope.WallElapsedNS != nil && *scope.WallElapsedNS < 0 || scope.SubStepElapsedNS != nil {
		return nativeInvalid("Invalid bounded GitHub scope observation")
	}
	scope.Coverage = "selected_client_completed_attempts; sub_steps_subprocesses_other_clients_unavailable"
	scope.ElapsedSemantics = "http_sums_can_exceed_wall; token_resolution_inclusive_can_contain_installation_http"
	scope.BuildCoverage = "unavailable"
	scope.HTTPBoundary, scope.TimingUnit = "do_body_close", "nanoseconds"
	validKey := func(key tracker.NativeGitHubKey) bool {
		return key.Stage == "merging" && slices.Contains([]string{"prepare", "land"}, key.Step) &&
			slices.Contains([]string{"search", "label issues", "repository issues", "issue comments", "issue field values", "issue dependencies", "issue reads", "pull requests", "reviews", "mutations", "workflow runs", "check run annotations", "check runs", "commit statuses", "other", "graphql", "app installation tokens"}, key.EndpointFamily) &&
			slices.Contains([]string{"200", "304", "429", "error", "fanout-deferred", "reserve-refused"}, key.Outcome)
	}
	for _, count := range scope.RESTCounts {
		if !validKey(count.NativeGitHubKey) || count.EndpointFamily == "graphql" || count.Count <= 0 {
			return nativeInvalid("Invalid GitHub request count")
		}
	}
	for _, timing := range scope.Timings {
		protocol := "rest"
		if timing.EndpointFamily == "graphql" {
			protocol = "graphql"
		}
		purposeValid := timing.EndpointFamily == "app installation tokens" && timing.QueryPurpose == "" || slices.Contains([]string{"fetch_issues_by_status_label", "fetch_repository_issues", "hydrate_issue", "hydrate_issue_comments", "hydrate_issue_dependencies", "hydrate_issue_fields", "hydrate_pull_request", "hydrate_pull_request_reviews", "hydrate_pull_request_checks", "hydrate_pull_request_statuses", "search_issue_metadata", "mutate_github_state", "workflow_runs", "check_run_annotations", "other", "graphql", "authenticate", "candidate_issues", "observed_status", "running_states", "epic_children", "issue_parents", "pull_requests", "pull_request_review_threads", "blocked_reasons", "issue_lookup", "project_item", "project_metadata", "assignees", "create_comment", "close_issue", "set_assignee", "remove_assignees", "update_project_field", "remove_project_item", "add_project_item", "merge_queue", "enqueue_pull_request", "dequeue_pull_request", "rate_limit_probe", "lane_signal_status", "onboarding_issue_discovery", "linked_issue_intake"}, timing.QueryPurpose)
		if !validKey(timing.NativeGitHubKey) || timing.Protocol != protocol || !slices.Contains([]string{"http_transport", "token_resolution_inclusive"}, timing.Boundary) || !purposeValid || timing.AttemptCount <= 0 || timing.TimedCount < 0 || timing.TimedCount > timing.AttemptCount || timing.FirstObservedAt.IsZero() || timing.LastObservedAt.Before(timing.FirstObservedAt) || timing.FirstObservedAt.Before(scope.StartedAt) || timing.LastObservedAt.After(scope.ObservedAt) {
			return nativeInvalid("Invalid fixed-key GitHub timing")
		}
		if timing.TimedCount == 0 {
			if timing.ElapsedSumNS != nil || timing.ElapsedMaxNS != nil {
				return nativeInvalid("Unobserved GitHub duration must be null")
			}
		} else if timing.ElapsedSumNS == nil || timing.ElapsedMaxNS == nil || *timing.ElapsedSumNS < 0 || *timing.ElapsedMaxNS < 0 || *timing.ElapsedMaxNS > *timing.ElapsedSumNS {
			return nativeInvalid("Invalid observed GitHub duration")
		}
	}
	return nil
}
