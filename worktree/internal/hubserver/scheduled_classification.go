package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
)

type ciTextBackend struct{ config *ConversationConfig }

func (b ciTextBackend) Generate(ctx context.Context, instructions, input string) (string, error) {
	if b.config == nil || b.config.Backend == nil {
		return "", errors.New("CI AI backend unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var text strings.Builder
	request := runner.AgentTurnRequest{Workspace: b.config.Workspace, Prompt: input, ToolInstructions: instructions, Model: b.config.Model, ReasoningEffort: b.config.ReasoningEffort,
		ReadOnly: true, TextOnly: true, MaxTurns: 1, TurnTimeout: 20 * time.Second, MaxDuration: 20 * time.Second}
	_, err := b.config.Backend.RunTurn(ctx, request, func(update runner.AgentUpdate) error {
		if update.Type == runner.AgentUpdateMessageDelta {
			if text.Len()+len(update.Delta) > progresscheck.MaxOutputBytes {
				return errors.New("CI AI output exceeds byte bound")
			}
			text.WriteString(update.Delta)
		}
		return nil
	})
	return text.String(), err
}

func readCIClassification(ctx context.Context, q nativeQueryer, scope nativeScope, input progresscheck.CIFailure) (progresscheck.CIDecision, bool, error) {
	var result progresscheck.CIDecision
	var head string
	err := q.QueryRowContext(ctx, `SELECT head_sha,outcome,target,reason FROM scheduled_ci_decisions
 WHERE organization_id=? AND project_id=? AND repository=? AND run_id=? AND run_attempt=? AND job_id=?`,
		scope.organization, scope.project, input.Repository, input.RunID, input.RunAttempt, input.JobID).Scan(&head, &result.Outcome, &result.Target, &result.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, fmt.Errorf("read CI classification: %w", err)
	}
	if head != input.HeadSHA {
		return result, false, nativePublicInvalid("CI classification commit differs from retained failure")
	}
	return result, true, result.Validate()
}

func (s *Service) classifyScheduledFailure(c echo.Context) error {
	scope := nativeRequestScope(c)
	if scope.credential.HostedRole == "reporter" {
		return s.nativeAPIError(c, &nativeError{Code: "insufficient_scope", status: http.StatusForbidden})
	}
	var input progresscheck.CIFailure
	if err := decodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := input.Validate(); err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid or oversized CI classification evidence"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), time.Minute)
	defer cancel()
	repository, err := policyRepository(ctx, s.database.reader, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if !strings.EqualFold(repository, input.Repository) {
		return s.nativeAPIError(c, nativePublicInvalid("CI classification repository differs from project"))
	}
	input.Repository = repository
	result, found, err := readCIClassification(ctx, s.database.reader, scope, input)
	if found || err != nil {
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	result, err = (progresscheck.AI{Backend: ciTextBackend{config: s.config.Conversation}}).ClassifyCI(ctx, input)
	if err != nil {
		if ctx.Err() != nil {
			return s.nativeAPIError(c, ctx.Err())
		}
		result = progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "AI classification unavailable or invalid; diagnose the retained job evidence through the source repair path."}
	}
	_, err = s.database.db.ExecContext(ctx, `INSERT INTO scheduled_ci_decisions
 (organization_id,project_id,repository,head_sha,run_id,run_attempt,job_id,outcome,target,reason,created_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, scope.organization, scope.project, input.Repository, input.HeadSHA, input.RunID, input.RunAttempt, input.JobID,
		result.Outcome, result.Target, result.Reason, s.config.now().UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if err != nil {
		return s.nativeAPIError(c, fmt.Errorf("store CI classification: %w", err))
	}
	result, _, err = readCIClassification(ctx, s.database.reader, scope, input)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
