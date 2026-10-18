package pause

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
)

var ErrIssueResolverUnavailable = errors.New("pause issue resolver is unavailable")

const DefaultEvaluationFailureThreshold = 3

type Result struct {
	Met    bool
	Detail string
}

type ExitStatus struct {
	ProjectID           string    `json:"project_id"`
	Reference           string    `json:"reference"`
	ResolverProjectID   string    `json:"resolver_project_id,omitempty"`
	Evaluable           bool      `json:"evaluable"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	NeedsAttention      bool      `json:"needs_attention"`
	EvaluatedAt         time.Time `json:"evaluated_at"`
}

func Evaluate(
	ctx context.Context,
	project globalconfig.Project,
	now time.Time,
	trackerRepository string,
	resolver connector.IssueReferenceResolver,
) (Result, error) {
	if !project.Paused {
		return Result{}, nil
	}
	if until := strings.TrimSpace(project.PausedUntil); until != "" {
		expiresAt, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return Result{}, fmt.Errorf("parse paused_until: %w", err)
		}
		if !now.Before(expiresAt) {
			return Result{Met: true, Detail: "paused_until reached at " + expiresAt.Format(time.RFC3339)}, nil
		}
		return Result{}, nil
	}

	issueRef := strings.TrimSpace(project.PausedUntilIssue)
	if issueRef == "" {
		return Result{}, nil
	}
	resolvedIssueRef := trackerIssueReference(issueRef, trackerRepository)
	if resolver == nil {
		return Result{}, ErrIssueResolverUnavailable
	}
	issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, []string{resolvedIssueRef})
	if err != nil {
		return Result{}, fmt.Errorf("fetch pause exit issue %s: %w", issueRef, err)
	}
	if len(issues) == 0 {
		return Result{}, fmt.Errorf("pause exit issue %s was not found", issueRef)
	}
	if issues[0].Closed {
		return Result{Met: true, Detail: "pause exit issue " + issueRef + " is closed"}, nil
	}
	return Result{}, nil
}

func trackerIssueReference(reference string, repository string) string {
	reference = strings.TrimSpace(reference)
	repository = strings.TrimSpace(repository)
	if reference == "" || repository == "" {
		return reference
	}
	if strings.HasPrefix(reference, "#") {
		return repository + reference
	}
	index := strings.LastIndex(reference, "#")
	slash := strings.LastIndex(repository, "/")
	if index <= 0 || slash < 0 {
		return reference
	}
	if strings.EqualFold(strings.TrimSpace(reference[:index]), strings.TrimSpace(repository[slash+1:])) {
		return repository + reference[index:]
	}
	return reference
}

func HeldLongerThan(project globalconfig.Project, now time.Time, duration time.Duration) bool {
	if !project.Paused ||
		strings.TrimSpace(project.PausedUntilIssue) != "" ||
		strings.TrimSpace(project.PausedUntil) != "" {
		return false
	}
	pausedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(project.PausedAt))
	if err != nil {
		return false
	}
	return now.Sub(pausedAt) > duration
}
