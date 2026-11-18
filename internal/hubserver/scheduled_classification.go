package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
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

func (s *Service) readCIClassification(ctx context.Context, scope hubscope.NativeScope, input progresscheck.CIFailure) (progresscheck.CIDecision, bool, error) {
	var result progresscheck.CIDecision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(policystore.ScheduledReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		result, err = owner.ReadScheduledDecision(ctx, scope.Project, input)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return result, false, nil
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return result, false, hubapi.NativePublicInvalid("CI classification commit differs from retained failure")
	}
	return result, err == nil, err
}

func (s *Service) scheduledRepository(ctx context.Context, scope hubscope.NativeScope) (string, error) {
	if s.config.TenantRepository == nil {
		return "", hubapi.NativeWorkItemStorageUnavailable()
	}
	var repository string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		record, err := r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if record.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		repository = record.Settings.CheckoutRepository
		if repository == "" {
			repository = record.Settings.Repository
		}
		return nil
	})
	return repository, hubapi.ProjectRecordError(err)
}

func (s *Service) classifyScheduledFailure(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.HostedRole == "reporter" {
		return s.nativeAPIError(c, &hubapi.NativeError{Code: "insufficient_scope", Status: http.StatusForbidden})
	}
	var input progresscheck.CIFailure
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := input.Validate(); err != nil {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Invalid or oversized CI classification evidence"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), time.Minute)
	defer cancel()
	repository, err := s.scheduledRepository(ctx, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if !strings.EqualFold(repository, input.Repository) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("CI classification repository differs from project"))
	}
	input.Repository = repository
	result, found, err := s.readCIClassification(ctx, scope, input)
	if found || err != nil {
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	evidence := gate.ScheduledEvidence{Schema: 1, Repository: repository, RunID: input.RunID, RunAttempt: input.RunAttempt, JobID: input.JobID, JobName: input.JobName, HeadSHA: input.HeadSHA, Conclusion: input.ExitStatus,
		OccurrenceKey: policy.Digest([]byte(input.RunID + "/" + input.RunAttempt + "/" + input.JobID)),
		RunURL:        "https://github.com/" + repository + "/actions/runs/" + input.RunID + "/attempts/" + input.RunAttempt,
		JobURL:        "https://github.com/" + repository + "/actions/jobs/" + input.JobID}
	if s.config.ImportBackend == nil || len(gate.ParseScheduledEvidence(evidence.Stamp())) != 1 {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("CI classification requires a verified failed job"))
	}
	request := GitHubImportRequest{Profile: "native", Repository: repository, Stage: "scheduled", Scheduled: &evidence}
	approval, err := s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if approval.Policy.ID != "" {
		contract, err := config.ResolvePolicyIssueContract(approval.Policy)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if workflow, branch, _, authorized := contract.ScheduledImportWorkflow(); authorized {
			request.ScheduledWorkflow, request.ScheduledBranch = workflow, branch
		}
	}
	verified, err := s.config.ImportBackend.FetchImportPage(ctx, request)
	if err != nil || verified.Scheduled == nil || verified.Scheduled.RunID != input.RunID || verified.Scheduled.RunAttempt != input.RunAttempt || verified.Scheduled.JobID != input.JobID || verified.Scheduled.HeadSHA != input.HeadSHA || verified.Scheduled.Repository != repository || verified.Scheduled.JobName != input.JobName || verified.Scheduled.Conclusion != input.ExitStatus {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("CI classification requires a verified failed job"))
	}
	identity, err := json.Marshal([]string{string(scope.Project), repository, input.RunID, input.RunAttempt, input.JobID})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := s.config.Clock().UTC()
		billing := s.database.plansService()
		plans := billing.Plans()
		var limit int64
		if plans == nil {
			defaults := hostedplans.PilotPlans()
			plans = &defaults
			limit = defaults.Plans[0].Allowances["api_mutations"]
		} else {
			entitlement, err := hubapi.FeatureResult(billing.EntitlementRecord(ctx, w, now))
			if err != nil {
				return err
			}
			limit = entitlement.Allowances["api_mutations"]
		}
		window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
		usage, err := w.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: scope.Organization, ID: "scheduled-ci/" + policy.Digest(identity), WindowStart: window, Metric: "api_mutations", Amount: 1}, limit)
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &hostedplans.LimitError{Resource: "api_mutations", Allowance: limit, Consumption: usage.Used}
		}
		if !usage.Applied {
			return &hubapi.NativeError{Code: "conflict", Status: http.StatusConflict}
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result, err = (progresscheck.AI{Backend: ciTextBackend{config: s.config.Conversation}}).ClassifyCI(ctx, input)
	if err != nil {
		if ctx.Err() != nil {
			return s.nativeAPIError(c, ctx.Err())
		}
		result = progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "AI classification unavailable or invalid; diagnose the retained job evidence through the source repair path."}
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(policystore.ScheduledWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if err := owner.StoreScheduledDecision(ctx, scope.Project, input, result, s.config.Clock().UTC()); err != nil {
			return err
		}
		if s.database.plansService().Plans() != nil {
			entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, w, s.config.Clock().UTC()))
			if err != nil {
				return err
			}
			used, err := w.CollaborationBytes(ctx)
			if err != nil {
				return err
			}
			if limit, limited := entitlement.Allowances["collaboration_bytes"]; limited && used > limit {
				return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used}
			}
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, fmt.Errorf("store CI classification: %w", err))
	}
	result, _, err = s.readCIClassification(ctx, scope, input)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
