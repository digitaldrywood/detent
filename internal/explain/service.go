package explain

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var ErrCorruptSource = errors.New("explanation evidence source is corrupt")

type SnapshotObservation struct {
	State     SourceState
	Snapshot  telemetry.Snapshot
	ExpiresAt *time.Time
}

type SnapshotSource interface {
	Snapshot(context.Context) (SnapshotObservation, error)
}

type WorkflowReader interface {
	IssueWorkflowTimeline(context.Context, store.IssueIdentity) (store.WorkflowTimeline, error)
}

type AttemptReader interface {
	ListActiveWorkAttempts(context.Context, store.WorkAttemptQuery) ([]store.WorkAttempt, error)
	ListRecentTerminalWorkAttempts(context.Context, store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error)
}

type SchedulerReader interface {
	ListIssueSchedulerDecisions(context.Context, store.IssueSchedulerDecisionQuery) ([]store.SchedulerDecision, error)
}

type SessionReader interface {
	LatestIssueAgentSession(context.Context, store.IssueIdentity) (store.IssueAgentSession, error)
}

type AdmissionReader interface {
	AdmissionProposalHistory(context.Context, string, string) ([]admissionmodel.Proposal, error)
}

type Dependencies struct {
	Snapshots SnapshotSource
	Workflow  WorkflowReader
	Attempts  AttemptReader
	Scheduler SchedulerReader
	Sessions  SessionReader
	Admission AdmissionReader
	Now       func() time.Time
}

type Service struct {
	snapshots SnapshotSource
	workflow  WorkflowReader
	attempts  AttemptReader
	scheduler SchedulerReader
	sessions  SessionReader
	admission AdmissionReader
	now       func() time.Time
}

type collectedEvidence struct {
	snapshot           SnapshotObservation
	snapshotIssues     []snapshotIssue
	workflow           []store.WorkflowPhaseEvent
	activeAttempts     []store.WorkAttempt
	terminalAttempts   []store.WorkAttempt
	schedulerDecisions []store.SchedulerDecision
	session            *store.IssueAgentSession
	admissionProposals []admissionmodel.Proposal
	sources            []SourceStatus
	configuredSources  int
	successfulSources  int
}

func New(deps Dependencies) *Service {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		snapshots: deps.Snapshots,
		workflow:  deps.Workflow,
		attempts:  deps.Attempts,
		scheduler: deps.Scheduler,
		sessions:  deps.Sessions,
		admission: deps.Admission,
		now:       now,
	}
}

func (s *Service) Explain(ctx context.Context, query Query) (IssueExplanation, error) {
	query = normalizeQuery(query)
	if query.ProjectID == "" {
		return IssueExplanation{}, ErrProjectRequired
	}
	if !queryHasIssueReference(query) {
		return IssueExplanation{}, ErrIssueReferenceNeeded
	}
	if err := ctx.Err(); err != nil {
		return IssueExplanation{}, err
	}

	observedAt := s.now().UTC()
	lookup := queryStoreIdentity(query)
	collected := collectedEvidence{}

	if err := s.collectSnapshot(ctx, observedAt, query, &collected); err != nil {
		return IssueExplanation{}, err
	}
	lookup = mergeLookupIdentity(lookup, identitiesFromSnapshot(collected.snapshotIssues))
	if err := s.collectWorkflow(ctx, lookup, &collected); err != nil {
		return IssueExplanation{}, err
	}
	lookup = mergeLookupIdentity(lookup, identitiesFromWorkflow(collected.workflow))
	if err := s.collectAttempts(ctx, lookup, &collected); err != nil {
		return IssueExplanation{}, err
	}
	lookup = mergeLookupIdentity(lookup, identitiesFromAttempts(collected.activeAttempts, collected.terminalAttempts))
	if err := s.collectScheduler(ctx, lookup, &collected); err != nil {
		return IssueExplanation{}, err
	}

	identity, err := resolveIdentity(query, collected)
	if err != nil {
		return IssueExplanation{}, err
	}
	resolvedLookup := store.IssueIdentity{
		ProjectID:  identity.ProjectID,
		IssueID:    identity.IssueID,
		Identifier: identity.Identifier,
		IssueURL:   identity.IssueURL,
	}
	if err := s.collectSession(ctx, resolvedLookup, &collected); err != nil {
		return IssueExplanation{}, err
	}
	if err := s.collectAdmission(ctx, resolvedLookup, &collected); err != nil {
		return IssueExplanation{}, err
	}

	found := collectedHasEvidence(collected)
	if !found && collected.configuredSources > 0 && collected.successfulSources == collected.configuredSources {
		return IssueExplanation{}, ErrNotFound
	}

	return buildExplanation(observedAt, identity, found, collected), nil
}

func (s *Service) collectSnapshot(ctx context.Context, now time.Time, query Query, collected *collectedEvidence) error {
	if s.snapshots == nil {
		collected.sources = append(collected.sources, unavailableSource("snapshot", "not_configured"))
		return nil
	}
	collected.configuredSources++
	observation, err := s.snapshots.Snapshot(ctx)
	if err != nil {
		if contextError(err) != nil {
			return contextError(err)
		}
		collected.sources = append(collected.sources, failedSource("snapshot", err))
		return nil
	}
	observation = normalizeSnapshotObservation(observation, now)
	if observation.State == SourceLive {
		collected.successfulSources++
	}
	collected.snapshot = observation
	if observation.State != SourceUnavailable && observation.State != SourceCorrupt {
		collected.snapshotIssues = matchingSnapshotIssues(observation.Snapshot, query)
	}
	status := SourceStatus{Name: "snapshot", State: observation.State}
	if observation.State == SourceLive && observation.Snapshot.Refresh.Degraded() {
		status.Code = "refresh_degraded"
	}
	collected.sources = append(collected.sources, status)
	return nil
}

func (s *Service) collectWorkflow(ctx context.Context, identity store.IssueIdentity, collected *collectedEvidence) error {
	if s.workflow == nil {
		collected.sources = append(collected.sources, unavailableSource("workflow", "not_configured"))
		return nil
	}
	collected.configuredSources++
	timeline, err := s.workflow.IssueWorkflowTimeline(ctx, identity)
	if err != nil {
		if contextError(err) != nil {
			return contextError(err)
		}
		collected.sources = append(collected.sources, failedSource("workflow", err))
		return nil
	}
	collected.successfulSources++
	collected.workflow = projectWorkflowEvents(identity.ProjectID, timeline.Events)
	collected.sources = append(collected.sources, availableSource("workflow"))
	return nil
}

func (s *Service) collectAttempts(ctx context.Context, identity store.IssueIdentity, collected *collectedEvidence) error {
	if s.attempts == nil {
		collected.sources = append(collected.sources,
			unavailableSource("active_attempt", "not_configured"),
			unavailableSource("terminal_attempt", "not_configured"),
		)
		return nil
	}

	collected.configuredSources += 2
	active, err := s.attempts.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: identity.ProjectID})
	if err != nil {
		if contextError(err) != nil {
			return contextError(err)
		}
		collected.sources = append(collected.sources, failedSource("active_attempt", err))
	} else {
		collected.successfulSources++
		collected.activeAttempts = matchingAttempts(identity, active)
		collected.sources = append(collected.sources, availableSource("active_attempt"))
	}

	terminal, err := s.attempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID:  identity.ProjectID,
		IssueID:    identity.IssueID,
		Identifier: identity.Identifier,
		IssueURL:   identity.IssueURL,
		Limit:      50,
	})
	if err != nil {
		if contextError(err) != nil {
			return contextError(err)
		}
		collected.sources = append(collected.sources, failedSource("terminal_attempt", err))
	} else {
		collected.successfulSources++
		collected.terminalAttempts = matchingAttempts(identity, terminal)
		collected.sources = append(collected.sources, availableSource("terminal_attempt"))
	}
	return nil
}

func (s *Service) collectScheduler(ctx context.Context, identity store.IssueIdentity, collected *collectedEvidence) error {
	if s.scheduler == nil {
		collected.sources = append(collected.sources, unavailableSource("scheduler", "not_configured"))
		return nil
	}
	collected.configuredSources++
	decisions, err := s.scheduler.ListIssueSchedulerDecisions(ctx, store.IssueSchedulerDecisionQuery{Identity: identity, Limit: 50})
	if err != nil {
		if contextError(err) != nil {
			return contextError(err)
		}
		collected.sources = append(collected.sources, failedSource("scheduler", err))
		return nil
	}
	collected.successfulSources++
	collected.schedulerDecisions = matchingSchedulerDecisions(identity, decisions)
	collected.sources = append(collected.sources, availableSource("scheduler"))
	return nil
}

func (s *Service) collectSession(ctx context.Context, identity store.IssueIdentity, collected *collectedEvidence) error {
	if s.sessions == nil {
		collected.sources = append(collected.sources, unavailableSource("session", "not_configured"))
		return nil
	}
	collected.configuredSources++
	session, err := s.sessions.LatestIssueAgentSession(ctx, identity)
	if errors.Is(err, store.ErrNotFound) {
		collected.successfulSources++
		collected.sources = append(collected.sources, availableSource("session"))
		return nil
	}
	if err != nil {
		if contextError(err) != nil {
			return contextError(err)
		}
		collected.sources = append(collected.sources, failedSource("session", err))
		return nil
	}
	collected.successfulSources++
	if strings.TrimSpace(session.ProjectID) == identity.ProjectID {
		collected.session = &session
	}
	collected.sources = append(collected.sources, availableSource("session"))
	return nil
}

func (s *Service) collectAdmission(ctx context.Context, identity store.IssueIdentity, collected *collectedEvidence) error {
	if s.admission == nil {
		collected.sources = append(collected.sources, unavailableSource("admission", "not_configured"))
		return nil
	}
	if identity.IssueID == "" {
		collected.sources = append(collected.sources, unavailableSource("admission", "issue_id_unresolved"))
		return nil
	}
	collected.configuredSources++
	proposals, err := s.admission.AdmissionProposalHistory(ctx, identity.ProjectID, identity.IssueID)
	if err != nil {
		if contextError(err) != nil {
			return contextError(err)
		}
		collected.sources = append(collected.sources, failedSource("admission", err))
		return nil
	}
	collected.successfulSources++
	for _, proposal := range proposals {
		if strings.TrimSpace(proposal.ProjectID) == identity.ProjectID && strings.TrimSpace(proposal.IssueID) == identity.IssueID {
			collected.admissionProposals = append(collected.admissionProposals, proposal)
		}
	}
	collected.sources = append(collected.sources, availableSource("admission"))
	return nil
}

func normalizeQuery(query Query) Query {
	return Query{
		ProjectID:  strings.TrimSpace(query.ProjectID),
		Reference:  normalizeIssueURL(query.Reference),
		IssueID:    strings.TrimSpace(query.IssueID),
		Identifier: strings.TrimSpace(query.Identifier),
		IssueURL:   normalizeIssueURL(query.IssueURL),
	}
}

func normalizeIssueURL(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}

func queryHasIssueReference(query Query) bool {
	return query.Reference != "" || query.IssueID != "" || query.Identifier != "" || query.IssueURL != ""
}

func queryStoreIdentity(query Query) store.IssueIdentity {
	identity := store.IssueIdentity{
		ProjectID:  query.ProjectID,
		IssueID:    query.IssueID,
		Identifier: query.Identifier,
		IssueURL:   query.IssueURL,
	}
	if identity.IssueID == "" && identity.Identifier == "" && identity.IssueURL == "" {
		identity.IssueID = query.Reference
		identity.Identifier = query.Reference
		identity.IssueURL = query.Reference
	}
	return identity
}

func mergeLookupIdentity(identity store.IssueIdentity, candidates []store.IssueIdentity) store.IssueIdentity {
	if len(candidates) > 0 && identity.IssueID != "" && identity.IssueID == identity.Identifier && identity.Identifier == identity.IssueURL {
		identity.IssueID = ""
		identity.Identifier = ""
		identity.IssueURL = ""
	}
	for _, candidate := range candidates {
		if identity.IssueID == "" {
			identity.IssueID = strings.TrimSpace(candidate.IssueID)
		}
		if identity.Identifier == "" {
			identity.Identifier = strings.TrimSpace(candidate.Identifier)
		}
		if identity.IssueURL == "" {
			identity.IssueURL = strings.TrimSpace(candidate.IssueURL)
		}
	}
	return identity
}

func availableSource(name string) SourceStatus {
	return SourceStatus{Name: name, State: SourceAvailable}
}

func unavailableSource(name string, code string) SourceStatus {
	return SourceStatus{Name: name, State: SourceUnavailable, Code: code}
}

func failedSource(name string, err error) SourceStatus {
	if errors.Is(err, ErrCorruptSource) {
		return SourceStatus{Name: name, State: SourceCorrupt, Code: "corrupt_source"}
	}
	return unavailableSource(name, "read_failed")
}

func contextError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}
