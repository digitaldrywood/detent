package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
)

func (s *Service) scheduledRepository(ctx context.Context, scope hubscope.NativeScope) (string, error) {
	return operatorcommands.ScheduledRepository(ctx, s.config.TenantRepository, scope)
}

func (s *Service) verifyScheduledClassification(ctx context.Context, scope hubscope.NativeScope, input progresscheck.CIFailure) error {
	invalid := hubapi.NativePublicInvalid("CI classification requires a verified job from the scheduled workflow")
	if s.config.ImportBackend == nil {
		return invalid
	}
	repository := input.Repository
	evidence := gate.ScheduledEvidence{
		Schema: 1, Repository: repository, HeadSHA: input.HeadSHA, RunID: input.RunID, RunAttempt: input.RunAttempt,
		JobID: input.JobID, JobName: input.JobName, Conclusion: input.ExitStatus,
		OccurrenceKey: policy.Digest([]byte(input.RunID + "/" + input.RunAttempt + "/" + input.JobID)),
		RunURL:        "https://github.com/" + repository + "/actions/runs/" + input.RunID + "/attempts/" + input.RunAttempt,
		JobURL:        "https://github.com/" + repository + "/actions/jobs/" + input.JobID,
	}
	request := GitHubImportRequest{Profile: "native", Repository: repository, Stage: githubcontract.ScheduledClassificationStage, Scheduled: &evidence}
	approval, err := s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
	if err != nil {
		return err
	}
	if approval.Policy.ID != "" {
		contract, err := config.ResolvePolicyIssueContract(approval.Policy)
		if err != nil {
			return err
		}
		if workflow, branch, _, ok := contract.ScheduledImportWorkflow(); ok {
			request.ScheduledWorkflow, request.ScheduledBranch = workflow, branch
		}
	}
	page, err := s.config.ImportBackend.FetchImportPage(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return invalid
	}
	job := page.Scheduled
	if job == nil || job.Repository != repository || job.HeadSHA != input.HeadSHA || job.RunID != input.RunID || job.RunAttempt != input.RunAttempt || job.JobID != input.JobID || job.JobName != input.JobName || job.Conclusion != input.ExitStatus {
		return invalid
	}
	return nil
}

func (s *Service) classifyScheduledFailure(c echo.Context) error {
	var text operatorcommands.CITextBackend
	if cfg := s.config.Conversation; cfg != nil {
		text = operatorcommands.CITextBackend{
			Backend: cfg.Backend, Workspace: cfg.Workspace, Model: cfg.Model, ReasoningEffort: cfg.ReasoningEffort,
		}
	}
	deps := operatorcommands.ScheduledCIDeps{
		Repository: s.config.TenantRepository,
		Plans:      s.database.plansService(),
		Authorize:  s.authorizeAccessScope,
		Verify:     s.verifyScheduledClassification,
		Now:        s.config.Clock,
		Text:       text,
		Error:      func(c echo.Context, err error) error { return s.nativeAPIError(c, err) },
		Invalid:    s.invalidAPIRequest,
	}
	return deps.Classify(c)
}
