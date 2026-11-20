package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/reports"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeRoundOwner struct {
	service *Service
	scope   hubscope.NativeScope
	item    tracker.NativeWorkItemID
}

type nativeRoundFacts struct {
	attempt          leasestore.Attempt
	itemRevision     int64
	evidenceSequence int64
	previousAttempt  string
	previousRound    string
	replay           eventstore.Receipt
	previous         eventstore.Receipt
	policy           policystore.Revision
	landing          eventstore.LandingReceipt
}

func validRoundPublication(input progresscheck.Publication) bool {
	hasSource := input.HeadSHA != "" || input.TreeSHA != ""
	if input.RoundID == "latest" || strings.TrimSpace(input.RoundID) == "" || len(input.RoundID) > 128 ||
		strings.TrimSpace(input.IdempotencyKey) == "" || len(input.IdempotencyKey) > 256 ||
		input.AttemptID == "" || input.PolicyID == "" || input.LeaseID == "" || input.FencingToken <= 0 ||
		!nativePhaseName.MatchString(input.Phase) || hasSource && !validNativeGitSource(input.HeadSHA, input.TreeSHA) ||
		input.ProviderTimeout != nil && !input.ProviderTimeout.Valid() || len(input.FailureSignature) > 256 || len(input.Reason) > 512 || len(input.Criteria) > progresscheck.MaxReceipts {
		return false
	}
	raw, err := json.Marshal(input.RoundInput)
	if err != nil || len(raw) > progresscheck.MaxOutputBytes {
		return false
	}
	switch input.Action {
	case progresscheck.RoundContinue:
		return nativePhaseName.MatchString(input.NextPhase) && input.HumanAction == ""
	case progresscheck.RoundDone:
		return hasSource && input.NextPhase == "" && input.HumanAction == ""
	case progresscheck.RoundHumanAction:
		return input.NextPhase == "" && strings.TrimSpace(input.HumanAction) != "" && len(input.HumanAction) <= 1024
	default:
		return false
	}
}

func (s *Service) publishNativeRoundVerdict(c echo.Context, scope hubscope.NativeScope, input progresscheck.Publication) error {
	if !validRoundPublication(input) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Round verdict requires the current attempt, a phase and a valid action with any required source"))
	}
	owner := nativeRoundOwner{s, scope, tracker.NativeWorkItemID(c.Param("item"))}
	ctx := s.runDetailContext(s.contentContext(c.Request().Context()))
	record, err := owner.publish(ctx, input)
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.JSON(http.StatusOK, record)
}

func (o nativeRoundOwner) operation() string {
	return nativeProgressReceiptOperation(o.scope, string(o.item)) + "/round"
}

func (o nativeRoundOwner) receipt(ctx context.Context, r tenantstore.Reader, round string) (eventstore.Receipt, error) {
	return r.Receipt(ctx, nativeProgressReceiptActor, o.operation(), round)
}

func (o nativeRoundOwner) load(ctx context.Context, row eventstore.Receipt) (progresscheck.RoundVerdict, error) {
	var record progresscheck.RoundVerdict
	raw, err := eventstore.NewContent(o.service.runDetailStoreFor(ctx)).Receipt(ctx, row)
	if err != nil {
		return record, err
	}
	if json.Unmarshal(raw, &record) != nil || record.RoundID != row.CommandKey || record.AttemptID == "" {
		return progresscheck.RoundVerdict{}, eventstore.ErrInvalidRecord
	}
	return record, nil
}

func (s *Service) getNativeRoundVerdict(c echo.Context, scope hubscope.NativeScope, round string) error {
	owner := nativeRoundOwner{s, scope, tracker.NativeWorkItemID(c.Param("item"))}
	ctx := s.runDetailContext(s.contentContext(c.Request().Context()))
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	var row eventstore.Receipt
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := s.nativeExecutionItem(ctx, r, scope, owner.item, false)
		if err != nil {
			return err
		}
		if err := s.authorizeProgressRunner(ctx, r, scope, leasestore.Attempt{}, nil); err != nil {
			return err
		}
		if round == "latest" {
			latest, ok := r.(leasestore.RoundVerdictReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			_, round, err = latest.LatestRoundVerdictID(ctx, scope.Project, item.ID)
			if err != nil || round == "" {
				return err
			}
		}
		row, err = owner.receipt(ctx, r, round)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	if row.CommandKey == "" {
		return c.JSON(http.StatusOK, nil)
	}
	record, err := owner.load(ctx, row)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, record)
}

func (o nativeRoundOwner) facts(ctx context.Context, r tenantstore.Reader, item corestore.WorkItem, attempt leasestore.Attempt, input progresscheck.Publication) (nativeRoundFacts, error) {
	facts := nativeRoundFacts{attempt: attempt, itemRevision: item.Revision}
	var err error
	facts.replay, err = o.receipt(ctx, r, input.RoundID)
	if err == nil || !errors.Is(err, corestore.ErrNotFound) {
		return facts, err
	}
	if attempt.Status != "running" && (attempt.Status != "succeeded" || input.Action != progresscheck.RoundDone) {
		return facts, hubapi.NativeStaleExecution("The item attempt has ended")
	}
	owner, ok := r.(leasestore.RoundVerdictReader)
	if !ok {
		return facts, hubapi.NativeWorkItemStorageUnavailable()
	}
	facts.previousAttempt, facts.previousRound, err = owner.LatestRoundVerdictID(ctx, o.scope.Project, item.ID)
	if err != nil {
		return facts, err
	}
	if facts.previousRound != "" {
		facts.previous, err = o.receipt(ctx, r, facts.previousRound)
		if err != nil {
			return facts, err
		}
	}
	evidence, ok := r.(eventstore.ProgressEvidenceReader)
	if !ok {
		return facts, hubapi.NativeWorkItemStorageUnavailable()
	}
	facts.evidenceSequence, err = evidence.ProgressEvidenceSequence(ctx, o.scope.Project, item.NativeID)
	if err != nil {
		return facts, err
	}
	facts.policy, err = r.ProjectPolicyRevision(ctx, o.scope.Project, input.PolicyID)
	if err != nil || input.Action != progresscheck.RoundDone {
		return facts, err
	}
	facts.landing, err = r.LandingReceipt(ctx, attempt.ID)
	if errors.Is(err, corestore.ErrNotFound) {
		err = nil
	}
	return facts, err
}

func (o nativeRoundOwner) readFacts(ctx context.Context, input progresscheck.Publication) (nativeRoundFacts, error) {
	var facts nativeRoundFacts
	repository, ok := o.service.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return facts, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, o.scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := o.service.nativeExecutionItem(ctx, r, o.scope, o.item, true)
		if err != nil {
			return err
		}
		identity, err := nativeExecutionRunner(ctx, r, o.scope, input.LeaseID)
		if err != nil {
			return err
		}
		attempt, err := o.service.nativeExecutionAttempt(ctx, r, o.scope, item, identity, input.AttemptID, input.PolicyID, input.Mutation)
		if err != nil {
			return err
		}
		facts, err = o.facts(ctx, r, item, attempt, input)
		return err
	})
	return facts, err
}

func (o nativeRoundOwner) publish(ctx context.Context, input progresscheck.Publication) (progresscheck.RoundVerdict, error) {
	facts, err := o.readFacts(ctx, input)
	if err != nil {
		return progresscheck.RoundVerdict{}, err
	}
	hash, err := hubapi.CommandHash(input)
	if err != nil {
		return progresscheck.RoundVerdict{}, err
	}
	if facts.replay.CommandKey != "" {
		return o.replay(ctx, facts.replay, hash)
	}
	record, err := o.assess(ctx, input, facts)
	if err != nil {
		return progresscheck.RoundVerdict{}, err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return progresscheck.RoundVerdict{}, err
	}
	receipt, err := eventstore.NewContent(o.service.runDetailStoreFor(ctx)).PrepareReceipt(ctx, eventstore.Receipt{
		OrganizationID: o.scope.Organization, ActorID: nativeProgressReceiptActor, Operation: o.operation(), CommandKey: input.RoundID,
		RequestHash: hash, CreatedAt: o.service.config.Clock().UTC(),
	}, raw)
	if err != nil {
		return progresscheck.RoundVerdict{}, err
	}
	replay, err := o.commit(ctx, input, facts, record, receipt)
	if err != nil {
		return progresscheck.RoundVerdict{}, err
	}
	if replay.CommandKey != "" {
		return o.replay(ctx, replay, hash)
	}
	return record, nil
}

func (o nativeRoundOwner) replay(ctx context.Context, row eventstore.Receipt, hash string) (progresscheck.RoundVerdict, error) {
	if row.RequestHash != hash {
		return progresscheck.RoundVerdict{}, mutation.ErrConflict
	}
	return o.load(ctx, row)
}

func (o nativeRoundOwner) commit(ctx context.Context, input progresscheck.Publication, facts nativeRoundFacts, record progresscheck.RoundVerdict, receipt eventstore.Receipt) (eventstore.Receipt, error) {
	var replay eventstore.Receipt
	err := o.service.config.TenantRepository.WriteTenant(ctx, o.scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		item, attempt, err := o.service.lockNativeExecution(ctx, w, o.scope, o.item, input.AttemptID, input.PolicyID, input.Mutation)
		if err != nil {
			return err
		}
		current, err := o.facts(ctx, w, item, attempt, input)
		if err != nil {
			return err
		}
		if current.replay.CommandKey != "" {
			replay = current.replay
			return nil
		}
		if current.attempt.Sequence != facts.attempt.Sequence || current.attempt.Status != facts.attempt.Status ||
			current.itemRevision != facts.itemRevision || current.evidenceSequence != facts.evidenceSequence ||
			current.previousRound != facts.previousRound || current.landing != facts.landing {
			return terminalProgressConflict()
		}
		owner, ok := w.(leasestore.ProgressVerdictWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		verdict := leasestore.ProgressVerdict{
			OrganizationID: o.scope.Organization, ProjectID: o.scope.Project, AttemptID: attempt.ID, AttemptSequence: attempt.Sequence,
			RoundID: &record.RoundID, EvidenceRevision: strconv.FormatInt(facts.evidenceSequence, 10),
			AttemptedStep: record.Phase, Success: record.Action == progresscheck.RoundDone, Outcome: record.Action, Reason: record.Reason,
		}
		if current.previousAttempt == attempt.ID {
			verdict.PreviousRoundID = &current.previousRound
		}
		if record.NextPhase != "" {
			verdict.NextStep = &record.NextPhase
		}
		if record.Disposition != "" {
			verdict.Disposition = &record.Disposition
		}
		if err := owner.RecordProgressVerdict(ctx, verdict); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	return replay, err
}

func (o nativeRoundOwner) assess(ctx context.Context, input progresscheck.Publication, facts nativeRoundFacts) (progresscheck.RoundVerdict, error) {
	record := progresscheck.RoundVerdict{
		RoundInput: input.RoundInput, LeaseID: input.LeaseID, FencingToken: input.FencingToken,
		AttemptID: input.AttemptID, PolicyID: input.PolicyID, PreviousRoundID: facts.previousRound,
		ObservedAt: o.service.config.Clock().UTC(),
	}
	if record.Reason == "" {
		record.Reason = record.FailureSignature
		if record.Reason == "" {
			record.Reason = strings.ReplaceAll(record.Action, "_", " ")
		}
	}
	issue, err := o.service.readWorkItem(ctx, o.scope, string(o.item))
	if err != nil {
		return record, err
	}
	content := eventstore.NewContent(o.service.runDetailStoreFor(ctx))
	descriptor, err := o.service.projectPolicyDescriptor(ctx, content, o.scope.Organization, o.scope.Project, facts.policy)
	if err != nil {
		return record, err
	}
	contract, err := config.ResolvePolicyIssueContract(descriptor)
	if err != nil {
		return record, err
	}
	criteria := contract.AcceptanceCriteria(issue.Body)
	record.CriteriaRevision, err = hubapi.CommandHash(criteria)
	if err != nil {
		return record, err
	}

	attempt, err := o.service.readNativeAttemptTenant(ctx, o.scope, string(o.item), input.AttemptID)
	if err != nil {
		return record, err
	}
	var command *gate.CommandResult
	if attempt.Runtime != nil {
		command = attempt.Runtime.Validation
	}
	exactCommand := command != nil && input.HeadSHA != "" && command.HeadSHA == input.HeadSHA && command.TreeSHA == input.TreeSHA
	if input.Action == progresscheck.RoundDone {
		record.Disposition, err = o.completion(ctx, input, facts, issue, attempt, content)
		if err != nil {
			return record, err
		}
		if record.Disposition == "" || command != nil && command.HeadSHA == input.HeadSHA && command.ExitCode != 0 {
			return record, hubapi.NativePublicInvalid("Completed round requires verified landing or no-change evidence for its source")
		}
	}
	if !exactCommand || command.ReceiptID == "" {
		return record, nil
	}
	statuses, previous, err := o.criterionHistory(ctx, facts.previous, input.PolicyID, record.CriteriaRevision, criteria)
	if err != nil {
		return record, err
	}
	for _, entry := range input.Criteria {
		if entry.RestatesImplementation || !slices.Contains(criteria, entry.Criterion) || len(entry.ReceiptIDs) != 1 || entry.ReceiptIDs[0] != command.ReceiptID {
			continue
		}
		previouslyVerified := statuses[entry.Criterion]
		if command.ExitCode == 0 && entry.Kind == gate.EvidenceReceipt && entry.HeadSHA == input.HeadSHA &&
			entry.TreeSHA == input.TreeSHA && strings.TrimSpace(entry.Behavior) != "" && strings.TrimSpace(entry.Reference) != "" {
			if !slices.Contains(record.VerifiedCriteria, entry.Criterion) {
				record.VerifiedCriteria = append(record.VerifiedCriteria, entry.Criterion)
			}
			record.Advanced = record.Advanced || !previouslyVerified
		}
		if command.ExitCode != 0 && entry.Kind == gate.EvidenceNotVerified {
			if !slices.Contains(record.FailedCriteria, entry.Criterion) {
				record.FailedCriteria = append(record.FailedCriteria, entry.Criterion)
			}
			record.Regressed = record.Regressed || previouslyVerified
		}
	}
	recordRoundProgress(&record, previous, command, previous.PolicyID == record.PolicyID && previous.CriteriaRevision == record.CriteriaRevision)

	return record, nil
}

func (o nativeRoundOwner) criterionHistory(ctx context.Context, row eventstore.Receipt, policyID, criteriaRevision string, criteria []string) (map[string]bool, progresscheck.RoundVerdict, error) {
	const maxRounds = 32
	previous := make(map[string]bool, len(criteria))
	var latest progresscheck.RoundVerdict
	for i := 0; i < maxRounds && row.CommandKey != ""; i++ {
		record, err := o.load(ctx, row)
		if err != nil {
			return nil, latest, err
		}
		if i == 0 {
			latest = record
		}
		if record.PolicyID != policyID || record.CriteriaRevision != criteriaRevision {
			break
		}
		for _, criterion := range criteria {
			if _, found := previous[criterion]; found {
				continue
			}
			if slices.Contains(record.FailedCriteria, criterion) {
				previous[criterion] = false
			} else if slices.Contains(record.VerifiedCriteria, criterion) {
				previous[criterion] = true
			}
		}
		if len(previous) == len(criteria) || record.PreviousRoundID == "" || i+1 == maxRounds {
			break
		}
		err = o.service.config.TenantRepository.ReadTenant(ctx, o.scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			row, err = o.receipt(ctx, r, record.PreviousRoundID)
			return err
		})
		if err != nil {
			return nil, latest, err
		}
	}
	return previous, latest, nil
}

func recordRoundProgress(record *progresscheck.RoundVerdict, previous progresscheck.RoundVerdict, command *gate.CommandResult, comparable bool) {
	if comparable {
		record.RecentDiagnostics = slices.Clone(previous.RecentDiagnostics)
	}
	if command.OutputTruncated || command.ExitCode == 0 {
		return
	}
	verifiedFailure, diagnostic := verifiedRoundDiagnostic(command)
	if diagnostic != "" && !slices.Contains(record.RecentDiagnostics, diagnostic) {
		record.DiagnosticReceipt = command.ReceiptID
		record.RecentDiagnostics = recentRoundEvidence(record.RecentDiagnostics, diagnostic)
	}
	exactFailure := len(record.FailedCriteria) > 0
	if !exactFailure && verifiedFailure != "" && record.Phase == "project_gate" && record.ProviderTimeout == nil {
		record.FailureSignature = verifiedFailure
		exactFailure = true
	}
	if !exactFailure || record.FailureSignature == "" || record.Advanced {
		return
	}
	if comparable {
		record.RecentFailures = slices.Clone(previous.RecentFailures)
	}
	record.CirclingCandidate = !record.Regressed && record.DiagnosticReceipt == "" && slices.Contains(record.RecentFailures, record.FailureSignature)
	record.RecentFailures = recentRoundEvidence(record.RecentFailures, record.FailureSignature)
}

func recentRoundEvidence(previous []string, evidence string) []string {
	const limit = 6
	previous = append(previous, evidence)
	return previous[max(0, len(previous)-limit):]
}

func verifiedRoundDiagnostic(command *gate.CommandResult) (failure, diagnostic string) {
	base := command.BaseReproduction
	if base == nil {
		return "", ""
	}
	head := gate.CommandObservation(*command)
	for _, finding := range command.Findings {
		if finding.CheckCommand != head.Command {
			continue
		}
		outcome, proof := failureattribution.Classify(base.HeadSHA, head.HeadSHA, finding.CheckDiagnostic, head, *base)
		if proof != nil {
			normalized := reports.NormalizeFailureSignature(proof.Diagnostic)
			failure = failureattribution.Digest(proof.Command + "\n" + normalized)
			return failure, failureattribution.Digest(outcome + "\n" + failure)
		}
	}
	return "", ""
}

func (o nativeRoundOwner) completion(ctx context.Context, input progresscheck.Publication, facts nativeRoundFacts, issue tracker.NativeIssue, attempt tracker.NativeAttempt, content *eventstore.Content) (string, error) {
	if facts.landing.AttemptID == input.AttemptID && facts.landing.MergeSHA != "" {
		landing, err := content.LandingReceipt(ctx, facts.landing)
		if err != nil {
			return "", err
		}
		if landing.Landed && !landing.GateFailed && landing.Refusal == "" && landing.HeadSHA == input.HeadSHA &&
			matchesCompletionTree(landing.Gate, input.TreeSHA) {
			return progresscheck.LandedDisposition, nil
		}
	}
	_, disposition := progresscheck.NativeTerminalRoute(tracker.NativeRecovery{Issue: issue}, attempt, input.PolicyID, false)
	if disposition == progresscheck.NoChangeDisposition && attempt.Finalization.NoChange.VerifiedSHA == input.HeadSHA &&
		matchesCompletionTree(attempt.Finalization.NoChange.Validation, input.TreeSHA) {
		return disposition, nil
	}
	return "", nil
}

func matchesCompletionTree(command *gate.CommandResult, tree string) bool {
	return command == nil || command.ExitCode == 0 && (command.TreeSHA == "" || command.TreeSHA == tree)
}
