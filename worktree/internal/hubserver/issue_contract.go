package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeIssueContract(ctx context.Context, q nativeQueryer, scope nativeScope) (config.IssueContract, error) {
	approval, _, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return config.IssueContract{}, err
	}
	if approval.Policy.Authored != nil && len(approval.Policy.Authored.Files) > 0 {
		workflow, err := projectPolicyWorkflow(ctx, string(scope.organization)+"/"+string(scope.project), approval.Policy)
		if err != nil {
			return config.IssueContract{}, err
		}
		return config.ResolveIssueContract(workflow.SharedPrompt)
	}
	return config.ResolvePolicyIssueContract(approval.Policy)
}

func nativeReporterContracts(ctx context.Context, q nativeQueryer, scope nativeScope) (config.ReporterContracts, error) {
	contract, err := nativeIssueContract(ctx, q, scope)
	if err != nil {
		return nil, err
	}
	return config.ResolveReporterContracts(contract.Context)
}

func nativeContractIssue(issue tracker.NativeIssue) connector.Issue {
	return connector.Issue{Title: issue.Title, Description: issue.Body, IssueContract: issue.IssueContract}
}

func nativeIssueContractConfig(ctx context.Context, q nativeQueryer, scope nativeScope) (config.Config, error) {
	approval, found, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return config.Config{}, err
	}
	if !found {
		return config.Default(), nil
	}
	workflow, err := projectPolicyWorkflow(ctx, string(scope.organization)+"/"+string(scope.project), approval.Policy)
	if err != nil {
		return config.Config{}, policyMismatch(err.Error())
	}
	return workflow.Config, nil
}

type nativeContractEvaluation struct {
	Allowed    bool
	ReasonCode string
	Reason     string
	Missing    []string
}

func evaluateNativeIssueContract(ctx context.Context, q nativeQueryer, scope nativeScope, issue tracker.NativeIssue) (nativeContractEvaluation, error) {
	allowed := nativeContractEvaluation{Allowed: true}
	if strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") {
		return nativeContractEvaluation{ReasonCode: "no_claimable_work", Reason: "The issue contains detent:no-dispatch; remove the marker to permit dispatch"}, nil
	}
	if issue.LinkedSource != nil && issue.LinkedSource.Status != "complete" || strings.EqualFold(issue.State, "Triage") || issue.IssueContract != nil && issue.IssueContract.Exempt {
		return allowed, nil
	}
	cfg, err := nativeIssueContractConfig(ctx, q, scope)
	if err != nil {
		return nativeContractEvaluation{}, err
	}
	target := cfg.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	if !strings.EqualFold(issue.State, target) {
		return allowed, nil
	}
	contract, err := nativeIssueContract(ctx, q, scope)
	if err != nil {
		return nativeContractEvaluation{}, err
	}
	if valid, err := releaseDeliveryContractCurrent(ctx, q, scope, issue, contract); err != nil {
		return nativeContractEvaluation{}, err
	} else if !valid {
		return nativeContractEvaluation{ReasonCode: "issue_contract_unsatisfied", Reason: "The verified release delivery contract changed; current approved policy and exact associated delivery are required"}, nil
	}
	evaluation := contract.Evaluate(nativeContractIssue(issue))
	var reasons []string
	if len(evaluation.Missing) > 0 {
		reasons = append(reasons, "Add or complete these required issue sections: "+strings.Join(evaluation.Missing, ", "))
	}
	if evaluation.NeedsConfirmation {
		reasons = append(reasons, "A human must confirm the success criteria by editing or saving the issue body")
	}
	if evaluation.Satisfied() {
		return allowed, nil
	}
	return nativeContractEvaluation{ReasonCode: "issue_contract_unsatisfied", Reason: strings.Join(reasons, "; "), Missing: evaluation.Missing}, nil
}

func readNativeIssueContractEvaluation(ctx context.Context, q nativeQueryer, scope nativeScope, id tracker.WorkItemID) (nativeContractEvaluation, error) {
	var native string
	if err := q.QueryRowContext(ctx, "SELECT native_id, project_id FROM issues WHERE id = ? AND organization_id = ?", id, scope.organization).Scan(&native, &scope.project); err != nil {
		return nativeContractEvaluation{}, err
	}
	issue, _, err := readNativeIssue(ctx, q, scope, native)
	if err != nil {
		return nativeContractEvaluation{}, err
	}
	return evaluateNativeIssueContract(ctx, q, scope, issue)
}

func nativeIssueContractClaimable(ctx context.Context, q nativeQueryer, scope nativeScope, id tracker.WorkItemID) (bool, string, error) {
	evaluation, err := readNativeIssueContractEvaluation(ctx, q, scope, id)
	return evaluation.Allowed, evaluation.Reason, err
}

func requireNativeIssueContractAtFiling(ctx context.Context, q nativeQueryer, scope nativeScope, issue tracker.NativeIssue) error {
	evaluation, err := evaluateNativeIssueContract(ctx, q, scope, issue)
	if err != nil {
		return fmt.Errorf("resolve issue contract: %w", err)
	}
	if len(evaluation.Missing) > 0 {
		return nativeWorkflowInvalid(evaluation.Reason + ". File into Backlog/Triage to draft it first.")
	}
	return nil
}

type nativeScheduledRepair struct {
	policyID     string
	evidence     gate.ScheduledEvidence
	importURL    string
	importBody   string
	importReason string
}

type scheduledImportsKey struct{}

func (s *Service) observeScheduledWebhook(ctx context.Context, delivery storedWebhook) context.Context {
	if delivery.EventType != "issues" || delivery.Action != "opened" {
		return ctx
	}
	var payload githubWebhookPayload
	if json.Unmarshal(delivery.Payload, &payload) != nil || payload.Issue == nil {
		return ctx
	}
	source, _, complete := normalizeWebhookIssue(payload.Issue)
	if !complete {
		return ctx
	}
	repository := webhookRepositoryFullName(payload.Repository)
	canonical, sourceRepository, _, err := tracker.ParseGitHubIssueURL(source.URL)
	if err != nil || !strings.EqualFold(repository, sourceRepository) {
		return ctx
	}
	rows, err := s.database.reader.QueryContext(ctx, `SELECT p.organization_id,p.id FROM projects p LEFT JOIN repositories r ON r.id=p.repository_id
WHERE p.deleted_at IS NULL AND p.profile='native' AND (lower(r.github_owner || '/' || r.github_name)=lower(?) OR lower(p.checkout_repository)=lower(?))
AND NOT EXISTS (SELECT 1 FROM issues i WHERE i.project_id=p.id AND (i.native_source_key=? OR (i.repository_id=p.repository_id AND i.github_number=?)))`, repository, repository, "github:"+canonical, source.Number)
	if err != nil {
		return ctx
	}
	defer rows.Close()
	var scopes []nativeScope
	for rows.Next() {
		var scope nativeScope
		if err := rows.Scan(&scope.organization, &scope.project); err != nil {
			return ctx
		}
		scopes = append(scopes, scope)
	}
	if errors.Join(rows.Err(), rows.Close()) != nil {
		return ctx
	}
	imports := make(map[string]*nativeScheduledRepair, len(scopes))
	for _, scope := range scopes {
		imports[string(scope.organization)+"/"+string(scope.project)+"/"+canonical] = s.observeImportedScheduledRepair(ctx, scope, linkedSnapshotFromSource(IssueSource{URL: canonical, Body: source.Body, AuthorID: source.AuthorID}, s.config.now()))
	}
	return context.WithValue(ctx, scheduledImportsKey{}, imports)
}

func (s *Service) observeImportedScheduledRepair(ctx context.Context, scope nativeScope, snapshot tracker.GitHubIssueSnapshot) *nativeScheduledRepair {
	origin, machine := issueorigin.Parse(snapshot.Body)
	if !machine || origin.Kind != "doctor" || origin.Instance != "github-actions" {
		return nil
	}
	result := &nativeScheduledRepair{importURL: snapshot.URL, importBody: snapshot.Body, importReason: "Scheduled CI import requires an approved repair delegation and verified GitHub Actions provenance"}
	if snapshot.Provenance.Provider != "github" || snapshot.Provenance.AuthorID != "github-actions[bot]" || strings.Contains(strings.ToLower(snapshot.Body), "detent:no-dispatch") || s.config.ImportBackend == nil {
		return result
	}
	approval, found, err := readProjectPolicy(ctx, s.database.reader, string(scope.organization)+"/"+string(scope.project))
	if err != nil || !found || approval.Policy.Authored == nil {
		return result
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil || contract.ScheduledImportBody() == "" {
		return result
	}
	result.importReason = "Scheduled CI import requires complete sections matching the approved bounded repair contract"
	digests := config.IssueContractSectionDigests(snapshot.Body)
	for section, digest := range config.IssueContractSectionDigests(contract.ScheduledImportBody()) {
		if digests[section] != digest {
			return result
		}
	}
	evidence := gate.ParseScheduledEvidence(snapshot.Body)
	_, sourceRepository, _, err := tracker.ParseGitHubIssueURL(snapshot.URL)
	repository, repoErr := policyRepository(ctx, s.database.reader, string(scope.organization)+"/"+string(scope.project))
	result.importReason = "Scheduled CI import requires a verified failed job from the configured repository CI workflow"
	if err != nil || repoErr != nil || len(evidence) != 1 || !strings.EqualFold(repository, sourceRepository) || !strings.EqualFold(repository, evidence[0].Repository) || !scheduledRepairOriginMatches(origin.Source, evidence[0]) {
		return result
	}
	workflow, branch, _, authorized := contract.ScheduledImportWorkflow()
	if !authorized {
		return result
	}
	page, err := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: repository, Stage: "scheduled", Scheduled: &evidence[0], ScheduledWorkflow: workflow, ScheduledBranch: branch})
	if err != nil || page.Scheduled == nil {
		return result
	}
	result.policyID, result.evidence, result.importReason = approval.Policy.ID, *page.Scheduled, ""
	return result
}

func (s *Service) observeScheduledRepair(ctx context.Context, scope nativeScope, request tracker.CreateIssue) *nativeScheduledRepair {
	origin, machine := issueorigin.Parse(request.Body)
	if !machine || origin.Kind != "doctor" || origin.Instance != "github-actions" || scope.credential.Scope == apiScopeWorker || request.GitHubIssueURL != "" || request.Provenance != nil || s.config.ImportBackend == nil {
		return nil
	}
	evidence := gate.ParseScheduledEvidence(request.Body)
	if len(evidence) != 1 || !scheduledRepairOriginMatches(origin.Source, evidence[0]) {
		return nil
	}
	approval, found, err := readProjectPolicy(ctx, s.database.reader, string(scope.organization)+"/"+string(scope.project))
	if err != nil || !found || approval.Policy.Authored == nil {
		return nil
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil || contract.ScheduledRepairBody() == "" {
		return nil
	}
	repository, err := policyRepository(ctx, s.database.reader, string(scope.organization)+"/"+string(scope.project))
	if err != nil || !strings.EqualFold(repository, evidence[0].Repository) {
		return nil
	}
	workflow, branch, _, authorized := contract.ScheduledRepairWorkflow()
	if !authorized {
		return nil
	}
	page, err := s.config.ImportBackend.FetchImportPage(ctx, GitHubImportRequest{Profile: "native", Repository: repository, Stage: "scheduled", Scheduled: &evidence[0], ScheduledWorkflow: workflow, ScheduledBranch: branch})
	if err != nil || page.Scheduled == nil {
		return nil
	}
	return &nativeScheduledRepair{policyID: approval.Policy.ID, evidence: *page.Scheduled}
}

func applyScheduledRepairContract(ctx context.Context, q nativeQueryer, scope nativeScope, issue *tracker.NativeIssue, now time.Time) {
	verified := scope.scheduledRepair
	if verified == nil || verified.importReason != "" {
		return
	}
	if verified.importURL != "" {
		verified.importReason = "Scheduled CI import authorization changed before creation or Todo is not dispatchable"
	}
	approval, found, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil || !found || approval.Policy.ID != verified.policyID || approval.Policy.Authored == nil {
		return
	}
	repository, err := policyRepository(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil || !strings.EqualFold(repository, verified.evidence.Repository) {
		return
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil {
		return
	}
	_, _, priority, authorized := contract.ScheduledRepairWorkflow()
	if !authorized {
		return
	}
	body := contract.ScheduledRepairBody()
	if verified.importURL != "" {
		body = contract.ScheduledImportBody()
		if issue.Body != verified.importBody {
			return
		}
	}
	if body == "" {
		return
	}
	origin, ok := issueorigin.Parse(issue.Body)
	if !ok || origin.Kind != "doctor" || origin.Instance != "github-actions" || !scheduledRepairOriginMatches(origin.Source, verified.evidence) {
		return
	}
	project, err := readNativeProject(ctx, q, scope)
	if err != nil {
		return
	}
	eligible := false
	for _, state := range project.States {
		eligible = eligible || state.Name == "Todo" && state.Dispatchable && !state.Terminal && !state.OperatorOnly
	}
	if !eligible {
		return
	}
	issue.State, issue.Priority = "Todo", new(priority)
	verified.importReason = ""
	issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(body)
	issue.IssueContract.RecordedAt = now
	body += fmt.Sprintf("\n\n## Verified CI occurrence\nRun: %s\nJob: %s\nTested commit: %s\nApproved policy: %s", verified.evidence.RunURL, verified.evidence.JobURL, verified.evidence.HeadSHA, verified.policyID)
	body += "\n\n## Diagnostic evidence (untrusted)\n\n> " + strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(issue.Body, "\r", "")), "\n", "\n> ")
	issue.Body = issueorigin.Stamp(body, origin) + verified.evidence.Stamp()
}

func scheduledRepairOriginMatches(source string, evidence gate.ScheduledEvidence) bool {
	runURL := "https://github.com/" + evidence.Repository + "/actions/runs/" + evidence.RunID
	return source == runURL || source == runURL+"/attempts/"+evidence.RunAttempt
}
