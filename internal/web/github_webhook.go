package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

const githubWebhookMaxBodyBytes = 2 << 20

func (s *Server) githubWebhook(c echo.Context) error {
	if s.refresher == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("orchestrator_unavailable", "Orchestrator is unavailable"))
	}

	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, githubWebhookMaxBodyBytes)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse("invalid_payload", "Webhook payload is invalid"))
	}
	target, err := githubWebhookTarget(req.Header.Get("X-GitHub-Event"), req.Header.Get("X-GitHub-Delivery"), body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse("invalid_payload", "Webhook payload is invalid"))
	}
	routes := s.githubWebhookRoutes(target.Repository)
	if len(routes) == 0 {
		return c.JSON(http.StatusNotFound, errorResponse("webhook_not_configured", "GitHub webhook is not configured for this project"))
	}
	validSignature := false
	for _, route := range routes {
		if !validGitHubWebhookSignature(route.secret, body, req.Header.Get("X-Hub-Signature-256")) {
			continue
		}
		validSignature = true
		if route.projectID != "" {
			target.ProjectIDs = append(target.ProjectIDs, route.projectID)
		}
	}
	if !validSignature {
		return c.JSON(http.StatusUnauthorized, errorResponse("invalid_signature", "Webhook signature is invalid"))
	}
	if !githubWebhookRefreshEvent(target.Event) {
		return c.JSON(http.StatusAccepted, RefreshResponse{
			RequestedAt: apiNow(),
			Operations:  []string{"webhook_ignored:" + target.Event},
		})
	}
	response, err := s.requestWebhookRefresh(req.Context(), target)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("orchestrator_unavailable", "Orchestrator is unavailable"))
	}
	if response.RequestedAt.IsZero() {
		response.RequestedAt = apiNow()
	}
	response.Operations = prependOperation(response.Operations, "webhook:"+target.Event)
	return c.JSON(http.StatusAccepted, response)
}

func githubWebhookRefreshEvent(event string) bool {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "issues", "label", "pull_request", "check_suite", "check_run":
		return true
	default:
		return false
	}
}

func validGitHubWebhookSignature(secret string, body []byte, signature string) bool {
	signature = strings.TrimSpace(signature)
	value, ok := strings.CutPrefix(signature, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func githubWebhookTarget(event string, deliveryID string, body []byte) (RefreshTarget, error) {
	var payload struct {
		Repository *struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Issue *struct {
			Number int `json:"number"`
		} `json:"issue"`
		PullRequest *struct {
			Number int `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
		CheckRun *struct {
			HeadSHA      string `json:"head_sha"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
			CheckSuite struct {
				HeadBranch string `json:"head_branch"`
			} `json:"check_suite"`
		} `json:"check_run"`
		CheckSuite *struct {
			HeadSHA      string `json:"head_sha"`
			HeadBranch   string `json:"head_branch"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_suite"`
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return RefreshTarget{}, err
	}
	target := RefreshTarget{
		Event:      strings.TrimSpace(event),
		DeliveryID: strings.TrimSpace(deliveryID),
	}
	if payload.Repository != nil {
		target.Repository = strings.TrimSpace(payload.Repository.FullName)
	}
	if payload.Issue != nil {
		target.IssueNumber = payload.Issue.Number
	}
	if payload.PullRequest != nil {
		target.PullRequestNumber = payload.PullRequest.Number
		target.SHA = strings.TrimSpace(payload.PullRequest.Head.SHA)
		target.Branch = strings.TrimSpace(payload.PullRequest.Head.Ref)
	}
	if payload.CheckSuite != nil {
		target.SHA = strings.TrimSpace(payload.CheckSuite.HeadSHA)
		target.Branch = strings.TrimSpace(payload.CheckSuite.HeadBranch)
		if len(payload.CheckSuite.PullRequests) > 0 {
			target.PullRequestNumber = payload.CheckSuite.PullRequests[0].Number
		}
	}
	if payload.CheckRun != nil {
		target.SHA = strings.TrimSpace(payload.CheckRun.HeadSHA)
		target.Branch = strings.TrimSpace(payload.CheckRun.CheckSuite.HeadBranch)
		if len(payload.CheckRun.PullRequests) > 0 {
			target.PullRequestNumber = payload.CheckRun.PullRequests[0].Number
		}
	}
	if target.SHA == "" {
		target.SHA = strings.TrimSpace(payload.SHA)
	}
	if target.Repository == "" {
		return RefreshTarget{}, echo.ErrBadRequest
	}
	if target.Event == "" {
		target.Event = "unknown"
	}
	return target, nil
}

type githubWebhookRoute struct {
	projectID string
	secret    string
}

func (s *Server) githubWebhookRoutes(repository string) []githubWebhookRoute {
	repository = strings.TrimSpace(repository)
	routes := []githubWebhookRoute{}
	if s.registry != nil && s.registry.Len() > 0 {
		for _, trackedProject := range s.registry.List() {
			workflow := trackedProject.Workflow().Config
			if workflow.Tracker.Kind != workflowconfig.TrackerGitHub && workflow.Tracker.Kind != workflowconfig.TrackerGitHubLocal {
				continue
			}
			configuredRepository := strings.TrimSpace(workflow.Tracker.Repository)
			if configuredRepository != "" && !strings.EqualFold(configuredRepository, repository) {
				continue
			}
			secret := s.resolveGitHubWebhookSecret(workflow.Tracker.GitHubWebhookSecret)
			if secret == "" {
				continue
			}
			routes = append(routes, githubWebhookRoute{projectID: string(trackedProject.ID()), secret: secret})
		}
		return routes
	}
	if secret := s.resolveGitHubWebhookSecret(s.githubWebhookSecret); secret != "" {
		routes = append(routes, githubWebhookRoute{secret: secret})
	}
	return routes
}

func (s *Server) resolveGitHubWebhookSecret(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "$") {
		return value
	}
	key := strings.TrimSpace(strings.Trim(value[1:], "{}"))
	if key == "" {
		return ""
	}
	return strings.TrimSpace(s.lookupEnv(key))
}

func (s *Server) requestWebhookRefresh(ctx context.Context, target RefreshTarget) (RefreshResponse, error) {
	if refresher, ok := s.refresher.(TargetedRefresher); ok {
		return refresher.RequestTargetedRefresh(ctx, target)
	}
	return s.refresher.RequestRefresh(ctx)
}

func prependOperation(operations []string, operation string) []string {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return operations
	}
	for _, existing := range operations {
		if existing == operation {
			return operations
		}
	}
	return append([]string{operation}, operations...)
}
