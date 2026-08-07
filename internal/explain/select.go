package explain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type snapshotIssue struct {
	issue  telemetry.Issue
	rank   int
	source string
}

type SnapshotIssueScope struct {
	IncludeCompleted bool
}

type SnapshotIssueSelection struct {
	Identity Identity
	Issue    telemetry.Issue
	Source   string
}

func normalizeSnapshotObservation(observation SnapshotObservation, now time.Time) SnapshotObservation {
	if observation.ExpiresAt == nil && !observation.Snapshot.LastKnownUntil.IsZero() {
		expiresAt := observation.Snapshot.LastKnownUntil.UTC()
		observation.ExpiresAt = &expiresAt
	}
	if observation.ExpiresAt != nil && now.After(observation.ExpiresAt.UTC()) {
		observation.State = SourceExpired
		return observation
	}
	if observation.State != "" {
		if (observation.State == SourceLive || observation.State == SourceLastKnown) && observation.Snapshot.GeneratedAt.IsZero() {
			observation.State = SourceCorrupt
		}
		return observation
	}
	if observation.Snapshot.GeneratedAt.IsZero() {
		observation.State = SourceUnavailable
		return observation
	}
	if observation.Snapshot.LastKnown {
		observation.State = SourceLastKnown
		return observation
	}
	observation.State = SourceLive
	return observation
}

func matchingSnapshotIssues(snapshot telemetry.Snapshot, query Query) []snapshotIssue {
	return matchingSnapshotIssuesInScope(snapshot, query, SnapshotIssueScope{IncludeCompleted: true})
}

func ResolveSnapshotIssue(snapshot telemetry.Snapshot, query Query, scope SnapshotIssueScope) (SnapshotIssueSelection, error) {
	query = normalizeQuery(query)
	if !queryHasIssueReference(query) {
		return SnapshotIssueSelection{}, ErrIssueReferenceNeeded
	}

	matches := matchingSnapshotIssuesInScope(snapshot, query, scope)
	if len(matches) == 0 {
		return SnapshotIssueSelection{}, ErrNotFound
	}
	identity, err := resolveIdentity(query, collectedEvidence{snapshotIssues: matches})
	if err != nil {
		return SnapshotIssueSelection{}, err
	}
	return SnapshotIssueSelection{Identity: identity, Issue: matches[0].issue, Source: matches[0].source}, nil
}

func matchingSnapshotIssuesInScope(snapshot telemetry.Snapshot, query Query, scope SnapshotIssueScope) []snapshotIssue {
	capacity := len(snapshot.BoardIssues) + len(snapshot.Pipeline) + len(snapshot.Running) + len(snapshot.Queue) + len(snapshot.Blocked)
	if scope.IncludeCompleted {
		capacity += len(snapshot.Completed)
	}
	candidates := make([]snapshotIssue, 0, capacity)
	for _, issue := range snapshot.BoardIssues {
		candidates = append(candidates, snapshotIssue{issue: issue, rank: 0, source: "board"})
	}
	for _, issue := range snapshot.Pipeline {
		candidates = append(candidates, snapshotIssue{issue: issue, rank: 1, source: "pipeline"})
	}
	for _, issue := range snapshot.Running {
		candidates = append(candidates, snapshotIssue{issue: issue.Issue, rank: 2, source: "running"})
	}
	for _, issue := range snapshot.Queue {
		candidates = append(candidates, snapshotIssue{issue: issue.Issue, rank: 3, source: "queue"})
	}
	for _, issue := range snapshot.Blocked {
		candidates = append(candidates, snapshotIssue{issue: issue.Issue, rank: 4, source: "blocked"})
	}
	if scope.IncludeCompleted {
		for _, issue := range snapshot.Completed {
			candidates = append(candidates, snapshotIssue{issue: issue.Issue, rank: 5, source: "completed"})
		}
	}

	matches := make([]snapshotIssue, 0, 1)
	for _, candidate := range candidates {
		projectID := strings.TrimSpace(candidate.issue.ProjectID)
		if projectID == "" {
			projectID = strings.TrimSpace(snapshot.Project.ID)
		}
		if query.ProjectID != "" && projectID != query.ProjectID {
			continue
		}
		if !queryMatchesIssue(query, candidate.issue) {
			continue
		}
		candidate.issue.ProjectID = projectID
		matches = append(matches, candidate)
	}
	slices.SortStableFunc(matches, compareSnapshotIssues)
	return matches
}

func compareSnapshotIssues(left snapshotIssue, right snapshotIssue) int {
	if left.rank != right.rank {
		return left.rank - right.rank
	}
	leftUpdated := issueObservationTime(left.issue)
	rightUpdated := issueObservationTime(right.issue)
	if !leftUpdated.Equal(rightUpdated) {
		if leftUpdated.After(rightUpdated) {
			return -1
		}
		return 1
	}
	return strings.Compare(issueIdentityKey(left.issue), issueIdentityKey(right.issue))
}

func queryMatchesIssue(query Query, issue telemetry.Issue) bool {
	values := []string{strings.TrimSpace(issue.ID), strings.TrimSpace(issue.Identifier), strings.TrimSpace(issue.URL)}
	for _, queryValue := range []string{query.IssueID, query.Identifier, query.IssueURL, query.Reference} {
		queryValue = strings.TrimSpace(queryValue)
		if queryValue == "" {
			continue
		}
		for _, value := range values {
			if queryValue == value {
				return true
			}
		}
		if issue.Number > 0 {
			number := strconv.Itoa(issue.Number)
			if queryValue == number || queryValue == "#"+number {
				return true
			}
		}
	}
	return false
}

func identitiesFromSnapshot(issues []snapshotIssue) []store.IssueIdentity {
	identities := make([]store.IssueIdentity, 0, len(issues))
	for _, candidate := range issues {
		identities = append(identities, store.IssueIdentity{
			ProjectID:  candidate.issue.ProjectID,
			IssueID:    candidate.issue.ID,
			Identifier: candidate.issue.Identifier,
			IssueURL:   candidate.issue.URL,
		})
	}
	return identities
}

func identitiesFromWorkflow(events []store.WorkflowPhaseEvent) []store.IssueIdentity {
	identities := make([]store.IssueIdentity, 0, len(events))
	for _, event := range events {
		identities = append(identities, store.IssueIdentity{ProjectID: event.ProjectID, IssueID: event.IssueID, Identifier: event.Identifier, IssueURL: event.IssueURL})
	}
	return identities
}

func identitiesFromAttempts(groups ...[]store.WorkAttempt) []store.IssueIdentity {
	identities := []store.IssueIdentity{}
	for _, attempts := range groups {
		for _, attempt := range attempts {
			identities = append(identities, store.IssueIdentity{ProjectID: attempt.ProjectID, IssueID: attempt.IssueID, Identifier: attempt.Identifier, IssueURL: attempt.IssueURL})
		}
	}
	return identities
}

func identitiesFromScheduler(decisions []store.SchedulerDecision) []store.IssueIdentity {
	identities := make([]store.IssueIdentity, 0, len(decisions))
	for _, decision := range decisions {
		identities = append(identities, store.IssueIdentity{ProjectID: decision.ProjectID, IssueID: decision.IssueID, Identifier: decision.Identifier, IssueURL: decision.IssueURL})
	}
	return identities
}

func projectWorkflowEvents(projectID string, events []store.WorkflowPhaseEvent) []store.WorkflowPhaseEvent {
	filtered := make([]store.WorkflowPhaseEvent, 0, len(events))
	for _, event := range events {
		if strings.TrimSpace(event.ProjectID) == projectID {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func matchingAttempts(identity store.IssueIdentity, attempts []store.WorkAttempt) []store.WorkAttempt {
	filtered := make([]store.WorkAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		if strings.TrimSpace(attempt.ProjectID) != identity.ProjectID {
			continue
		}
		if issueValuesMatch(identity, attempt.IssueID, attempt.Identifier, attempt.IssueURL) {
			filtered = append(filtered, attempt)
		}
	}
	return filtered
}

func matchingSchedulerDecisions(identity store.IssueIdentity, decisions []store.SchedulerDecision) []store.SchedulerDecision {
	filtered := make([]store.SchedulerDecision, 0, len(decisions))
	for _, decision := range decisions {
		if strings.TrimSpace(decision.ProjectID) != identity.ProjectID {
			continue
		}
		if issueValuesMatch(identity, decision.IssueID, decision.Identifier, decision.IssueURL) {
			filtered = append(filtered, decision)
		}
	}
	return filtered
}

func issueValuesMatch(identity store.IssueIdentity, issueID string, identifier string, issueURL string) bool {
	pairs := [][2]string{
		{identity.IssueID, issueID},
		{identity.Identifier, identifier},
		{identity.IssueURL, issueURL},
	}
	for _, pair := range pairs {
		if strings.TrimSpace(pair[0]) != "" && strings.TrimSpace(pair[0]) == strings.TrimSpace(pair[1]) {
			return true
		}
	}
	return false
}

func resolveIdentity(query Query, collected collectedEvidence) (Identity, error) {
	projectIDs := map[string]struct{}{}
	issueIDs := map[string]struct{}{}
	identifiers := map[string]struct{}{}
	issueURLs := map[string]struct{}{}
	add := func(identity store.IssueIdentity) {
		addIdentityValue(projectIDs, identity.ProjectID)
		addIdentityValue(issueIDs, identity.IssueID)
		addIdentityValue(identifiers, identity.Identifier)
		addIdentityValue(issueURLs, identity.IssueURL)
	}
	if query.IssueID != "" || query.Identifier != "" || query.IssueURL != "" {
		add(store.IssueIdentity{ProjectID: query.ProjectID, IssueID: query.IssueID, Identifier: query.Identifier, IssueURL: query.IssueURL})
	}
	for _, identity := range identitiesFromSnapshot(collected.snapshotIssues) {
		add(identity)
	}
	for _, identity := range identitiesFromWorkflow(collected.workflow) {
		add(identity)
	}
	for _, identity := range identitiesFromAttempts(collected.activeAttempts, collected.terminalAttempts) {
		add(identity)
	}
	for _, identity := range identitiesFromScheduler(collected.schedulerDecisions) {
		add(identity)
	}
	for _, field := range []struct {
		name   string
		values map[string]struct{}
	}{
		{name: "project_id", values: projectIDs},
		{name: "issue_id", values: issueIDs},
		{name: "identifier", values: identifiers},
		{name: "issue_url", values: issueURLs},
	} {
		if len(field.values) > 1 {
			return Identity{}, &AmbiguousIdentityError{ProjectID: query.ProjectID, Field: field.name, Values: identityValues(field.values)}
		}
	}

	identity := Identity{
		ProjectID:  firstIdentityValue(projectIDs, query.ProjectID),
		IssueID:    firstIdentityValue(issueIDs, query.IssueID),
		Identifier: firstIdentityValue(identifiers, query.Identifier),
		IssueURL:   firstIdentityValue(issueURLs, query.IssueURL),
	}
	if len(collected.snapshotIssues) > 0 {
		identity.Number = collected.snapshotIssues[0].issue.Number
		identity.Title = strings.TrimSpace(collected.snapshotIssues[0].issue.Title)
	}
	return identity, nil
}

func addIdentityValue(values map[string]struct{}, value string) {
	if value = strings.TrimSpace(value); value != "" {
		values[value] = struct{}{}
	}
}

func identityValues(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func firstIdentityValue(values map[string]struct{}, fallback string) string {
	for value := range values {
		return value
	}
	return strings.TrimSpace(fallback)
}

func collectedHasEvidence(collected collectedEvidence) bool {
	return len(collected.snapshotIssues) > 0 ||
		len(collected.workflow) > 0 ||
		len(collected.activeAttempts) > 0 ||
		len(collected.terminalAttempts) > 0 ||
		len(collected.schedulerDecisions) > 0 ||
		collected.session != nil ||
		len(collected.admissionProposals) > 0
}

func buildExplanation(observedAt time.Time, identity Identity, found bool, collected collectedEvidence) IssueExplanation {
	selectedAttempt, selection := selectAttempt(collected.activeAttempts, collected.terminalAttempts)
	transition := selectTransition(collected.workflow)
	eligibility := buildEligibility(collected)
	sessions := buildSessions(selectedAttempt, selection, collected)
	pullRequest := selectPullRequest(identity.ProjectID, selectedAttempt, selection, transition, collected)
	gate := selectGate(selectedAttempt, selection, collected)
	lane := selectCurrentLane(collected, observedAt)

	explanation := IssueExplanation{
		Schema:           SchemaVersion,
		Found:            found,
		ObservedAt:       observedAt,
		Identity:         identity,
		CurrentLane:      lane,
		LatestTransition: transition,
		Eligibility:      eligibility,
		Attempt:          attemptModel(selectedAttempt, selection),
		Sessions:         sessions,
		PullRequest:      pullRequest,
		RequiredGate:     gate,
		Sources:          append([]SourceStatus(nil), collected.sources...),
	}
	explanation.Evidence = explanationEvidence(explanation, collected)
	return explanation
}

func selectCurrentLane(collected collectedEvidence, observedAt time.Time) Lane {
	state := collected.snapshot.State
	if state == "" {
		state = sourceState(collected.sources, "snapshot")
	}
	lane := Lane{
		Freshness: state,
		Degraded:  state != SourceLive,
	}
	if state == SourceUnavailable || state == SourceCorrupt || len(collected.snapshotIssues) == 0 {
		return lane
	}
	issue := collected.snapshotIssues[0].issue
	lane.Name = strings.TrimSpace(issue.State)
	if !collected.snapshot.Snapshot.GeneratedAt.IsZero() {
		observedAt := collected.snapshot.Snapshot.GeneratedAt.UTC()
		lane.ObservedAt = &observedAt
		lane.EvidenceID = snapshotEvidenceID(issue.ProjectID, collected.snapshot.Snapshot)
	}
	if collected.snapshot.Snapshot.Refresh.Degraded() || collected.snapshot.Snapshot.Refresh.Stale(observedAt) {
		lane.Degraded = true
	}
	return lane
}

func selectTransition(events []store.WorkflowPhaseEvent) *Transition {
	var selected *store.WorkflowPhaseEvent
	for index := range events {
		event := &events[index]
		if event.PhaseType != store.WorkflowPhaseTypeLane || !strings.EqualFold(strings.TrimSpace(event.Status), "entered") {
			continue
		}
		if selected == nil || event.StartedAt.After(selected.StartedAt) || (event.StartedAt.Equal(selected.StartedAt) && event.ID > selected.ID) {
			selected = event
		}
	}
	if selected == nil {
		return nil
	}
	metadata, parsed := provenance.Parse(selected.MetadataJSON)
	transitionProvenance := Provenance{State: SourceCorrupt, Origin: string(provenance.OriginUnknown)}
	var actor *Actor
	if parsed {
		transitionProvenance.State = SourceAvailable
		transitionProvenance.Origin = string(metadata.Provenance.Origin)
		if metadata.Provenance.Actor != nil {
			actor = &Actor{Login: strings.TrimSpace(metadata.Provenance.Actor.Login), Kind: strings.TrimSpace(metadata.Provenance.Actor.Kind)}
		}
		if metadata.Admission != nil {
			transitionProvenance.Admission = &Admission{ProposalID: strings.TrimSpace(metadata.Admission.ProposalID), Attributed: metadata.Admission.Attributed}
		}
	}
	source := strings.TrimSpace(selected.EndpointFamily)
	if source == "" {
		source = "workflow"
	}
	return &Transition{
		EvidenceID: fmt.Sprintf("workflow:%d", selected.ID),
		From:       strings.TrimSpace(selected.PreviousPhaseName),
		To:         strings.TrimSpace(selected.PhaseName),
		At:         selected.StartedAt.UTC(),
		Source:     source,
		Actor:      actor,
		Reason:     strings.TrimSpace(selected.Reason),
		Provenance: transitionProvenance,
		PRNumber:   selected.PRNumber,
		SessionID:  selected.SessionID,
	}
}

func buildEligibility(collected collectedEvidence) Eligibility {
	decisions := make([]EligibilityDecision, 0, len(collected.schedulerDecisions)+len(collected.admissionProposals))
	for _, decision := range collected.schedulerDecisions {
		state := EligibilityUnknown
		switch {
		case decision.Selected || decision.Result == store.SchedulerDecisionResultSelected:
			state = EligibilityEligible
		case decision.Result == store.SchedulerDecisionResultSkipped:
			state = EligibilityRefused
		}
		if state == EligibilityUnknown {
			continue
		}
		decisions = append(decisions, EligibilityDecision{
			EvidenceID: fmt.Sprintf("scheduler:%d", decision.ID),
			Source:     "scheduler",
			State:      state,
			Outcome:    string(decision.Result),
			Reason:     firstNonEmpty(decision.WaitReason, decision.Reason),
			At:         decision.DecisionAt.UTC(),
		})
	}
	for _, proposal := range collected.admissionProposals {
		state := EligibilityUnknown
		switch proposal.Status {
		case admissionmodel.ProposalAccepted:
			state = EligibilityEligible
		case admissionmodel.ProposalRejected:
			state = EligibilityRefused
		}
		if state == EligibilityUnknown {
			continue
		}
		at := proposal.ResolvedAt
		if at.IsZero() {
			at = proposal.CreatedAt
		}
		decisions = append(decisions, EligibilityDecision{
			EvidenceID: "admission:" + strings.TrimSpace(proposal.ID),
			Source:     "admission",
			State:      state,
			Outcome:    string(proposal.Status),
			Reason:     strings.TrimSpace(proposal.ResolutionReason),
			At:         at.UTC(),
		})
	}
	slices.SortStableFunc(decisions, compareEligibilityDecisions)
	eligibility := Eligibility{
		State:    EligibilityUnknown,
		Refusals: []EligibilityDecision{},
		Source:   combinedSourceState(collected.sources, "scheduler", "admission"),
	}
	if len(decisions) > 0 {
		latest := decisions[0]
		eligibility.Latest = &latest
		eligibility.State = latest.State
	}
	for _, decision := range decisions {
		if decision.State == EligibilityRefused {
			eligibility.Refusals = append(eligibility.Refusals, decision)
		}
	}
	return eligibility
}

func compareEligibilityDecisions(left EligibilityDecision, right EligibilityDecision) int {
	if !left.At.Equal(right.At) {
		if left.At.After(right.At) {
			return -1
		}
		return 1
	}
	if left.Source != right.Source {
		if left.Source == "scheduler" {
			return -1
		}
		if right.Source == "scheduler" {
			return 1
		}
	}
	return -strings.Compare(left.EvidenceID, right.EvidenceID)
}

func selectAttempt(active []store.WorkAttempt, terminal []store.WorkAttempt) (*store.WorkAttempt, string) {
	var selected *store.WorkAttempt
	for index := range active {
		attempt := &active[index]
		if selected == nil || attempt.StartedAt.After(selected.StartedAt) || (attempt.StartedAt.Equal(selected.StartedAt) && attempt.ID > selected.ID) {
			selected = attempt
		}
	}
	if selected != nil {
		return selected, "active"
	}
	for index := range terminal {
		attempt := &terminal[index]
		if selected == nil || attempt.CompletedAt.After(selected.CompletedAt) || (attempt.CompletedAt.Equal(selected.CompletedAt) && attempt.ID > selected.ID) {
			selected = attempt
		}
	}
	if selected != nil {
		return selected, "latest_terminal"
	}
	return nil, ""
}

func attemptModel(attempt *store.WorkAttempt, selection string) *Attempt {
	if attempt == nil {
		return nil
	}
	model := &Attempt{
		EvidenceID:        fmt.Sprintf("attempt:%d", attempt.ID),
		ID:                attempt.ID,
		Selection:         selection,
		Status:            string(attempt.Status),
		TerminalState:     string(attempt.TerminalState),
		AttemptNumber:     attempt.AttemptNumber,
		Lane:              strings.TrimSpace(attempt.Lane),
		Phase:             strings.TrimSpace(attempt.Phase),
		StatusMessage:     strings.TrimSpace(attempt.StatusMessage),
		WaitReason:        strings.TrimSpace(attempt.WaitReason),
		StartedAt:         attempt.StartedAt.UTC(),
		DetentSessionID:   attempt.DetentSessionID,
		ProviderSessionID: strings.TrimSpace(attempt.ProviderSessionID),
	}
	if !attempt.HeartbeatAt.IsZero() {
		heartbeatAt := attempt.HeartbeatAt.UTC()
		model.HeartbeatAt = &heartbeatAt
	}
	if !attempt.CompletedAt.IsZero() {
		completedAt := attempt.CompletedAt.UTC()
		model.CompletedAt = &completedAt
	}
	return model
}

func buildSessions(attempt *store.WorkAttempt, selection string, collected collectedEvidence) Sessions {
	sessions := Sessions{Source: combinedSourceState(collected.sources, "active_attempt", "terminal_attempt", "session")}
	if attempt != nil && attempt.DetentSessionID > 0 {
		sessions.Detent = &Session{
			EvidenceID: fmt.Sprintf("session:%d", attempt.DetentSessionID),
			ID:         strconv.FormatInt(attempt.DetentSessionID, 10),
			Backend:    strings.TrimSpace(attempt.WorkerType),
			Selection:  selection,
		}
	}
	if attempt != nil && strings.TrimSpace(attempt.ProviderSessionID) != "" {
		providerID := strings.TrimSpace(attempt.ProviderSessionID)
		sessions.Provider = &Session{
			EvidenceID: providerEvidenceID(attempt.WorkerType, providerID),
			ID:         providerID,
			Backend:    strings.TrimSpace(attempt.WorkerType),
			Selection:  selection,
		}
	}
	if collected.session != nil {
		completedAt := collected.session.CompletedAt.UTC()
		if sessions.Detent == nil && collected.session.DetentSessionID > 0 {
			sessions.Detent = &Session{
				EvidenceID:  fmt.Sprintf("session:%d", collected.session.DetentSessionID),
				ID:          strconv.FormatInt(collected.session.DetentSessionID, 10),
				Backend:     strings.TrimSpace(collected.session.AgentBackendKind),
				Selection:   "latest_completed",
				CompletedAt: &completedAt,
			}
		}
		providerID := firstNonEmpty(collected.session.ProviderThreadID, collected.session.ProviderSessionID)
		if sessions.Provider == nil && providerID != "" {
			sessions.Provider = &Session{
				EvidenceID:  providerEvidenceID(collected.session.AgentBackendKind, providerID),
				ID:          providerID,
				Backend:     strings.TrimSpace(collected.session.AgentBackendKind),
				Selection:   "latest_completed",
				CompletedAt: &completedAt,
			}
		}
	}
	if sessions.Detent != nil || sessions.Provider != nil {
		sessions.Source = SourceAvailable
	}
	return sessions
}

func selectPullRequest(projectID string, attempt *store.WorkAttempt, selection string, transition *Transition, collected collectedEvidence) *PullRequest {
	snapshotPR := snapshotPullRequest(projectID, collected)
	attemptPR := attemptPullRequest(projectID, attempt, selection)
	latestScheduler := latestSchedulerPR(projectID, collected.schedulerDecisions)
	workflowPR := workflowPullRequest(projectID, collected.workflow, transition)

	switch collected.snapshot.State {
	case SourceLive:
		return firstPullRequest(snapshotPR, attemptPR, latestScheduler, workflowPR)
	case SourceLastKnown:
		if selection == "active" {
			return firstPullRequest(attemptPR, snapshotPR, latestScheduler, workflowPR)
		}
		return firstPullRequest(snapshotPR, attemptPR, latestScheduler, workflowPR)
	case SourceExpired:
		return firstPullRequest(attemptPR, latestScheduler, workflowPR, snapshotPR)
	default:
		return firstPullRequest(attemptPR, latestScheduler, workflowPR)
	}
}

func snapshotPullRequest(projectID string, collected collectedEvidence) *PullRequest {
	if len(collected.snapshotIssues) == 0 || collected.snapshotIssues[0].issue.PullRequest == nil {
		return nil
	}
	pr := collected.snapshotIssues[0].issue.PullRequest
	if pr.Number <= 0 {
		return nil
	}
	observedAt := collected.snapshot.Snapshot.GeneratedAt.UTC()
	return &PullRequest{
		EvidenceID: pullRequestEvidenceID(projectID, int64(pr.Number)),
		Number:     int64(pr.Number),
		URL:        strings.TrimSpace(pr.URL),
		State:      strings.TrimSpace(pr.State),
		HeadSHA:    strings.TrimSpace(pr.HeadSHA),
		Source:     "snapshot",
		ObservedAt: timePointer(observedAt),
	}
}

func attemptPullRequest(projectID string, attempt *store.WorkAttempt, selection string) *PullRequest {
	if attempt == nil || attempt.PRNumber == nil || *attempt.PRNumber <= 0 {
		return nil
	}
	at := attempt.CompletedAt
	if selection == "active" {
		at = attempt.HeartbeatAt
		if at.IsZero() {
			at = attempt.StartedAt
		}
	}
	return &PullRequest{
		EvidenceID: pullRequestEvidenceID(projectID, *attempt.PRNumber),
		Number:     *attempt.PRNumber,
		Source:     "attempt",
		ObservedAt: timePointer(at.UTC()),
	}
}

func latestSchedulerPR(projectID string, decisions []store.SchedulerDecision) *PullRequest {
	var selected *store.SchedulerDecision
	for index := range decisions {
		decision := &decisions[index]
		if decision.PRNumber == nil || *decision.PRNumber <= 0 {
			continue
		}
		if selected == nil || decision.DecisionAt.After(selected.DecisionAt) || (decision.DecisionAt.Equal(selected.DecisionAt) && decision.ID > selected.ID) {
			selected = decision
		}
	}
	if selected == nil {
		return nil
	}
	return &PullRequest{
		EvidenceID: pullRequestEvidenceID(projectID, *selected.PRNumber),
		Number:     *selected.PRNumber,
		Source:     "scheduler",
		ObservedAt: timePointer(selected.DecisionAt.UTC()),
	}
}

func workflowPullRequest(projectID string, events []store.WorkflowPhaseEvent, transition *Transition) *PullRequest {
	var selected *store.WorkflowPhaseEvent
	for index := range events {
		event := &events[index]
		if event.PRNumber == nil || *event.PRNumber <= 0 {
			continue
		}
		if selected == nil || event.StartedAt.After(selected.StartedAt) || (event.StartedAt.Equal(selected.StartedAt) && event.ID > selected.ID) {
			selected = event
		}
	}
	if selected == nil && transition != nil && transition.PRNumber != nil {
		return &PullRequest{
			EvidenceID: pullRequestEvidenceID(projectID, *transition.PRNumber),
			Number:     *transition.PRNumber,
			Source:     "workflow",
			ObservedAt: timePointer(transition.At),
		}
	}
	if selected == nil {
		return nil
	}
	return &PullRequest{
		EvidenceID: pullRequestEvidenceID(projectID, *selected.PRNumber),
		Number:     *selected.PRNumber,
		Source:     "workflow",
		ObservedAt: timePointer(selected.StartedAt.UTC()),
	}
}

func firstPullRequest(candidates ...*PullRequest) *PullRequest {
	for _, candidate := range candidates {
		if candidate != nil {
			return candidate
		}
	}
	return nil
}

func selectGate(attempt *store.WorkAttempt, selection string, collected collectedEvidence) Gate {
	snapshotGate, snapshotRecorded := gateFromSnapshot(collected)
	attemptGate, attemptRecorded := gateFromAttempt(attempt, selection)

	if collected.snapshot.State == SourceLive && snapshotRecorded && gateIsUsable(snapshotGate) {
		return snapshotGate
	}
	if selection == "active" && attemptRecorded && gateIsUsable(attemptGate) {
		return attemptGate
	}
	if collected.snapshot.State == SourceLive && snapshotRecorded {
		return snapshotGate
	}
	if collected.snapshot.State == SourceLastKnown && snapshotRecorded {
		return snapshotGate
	}
	if attemptRecorded {
		return attemptGate
	}
	if collected.snapshot.State == SourceExpired && snapshotRecorded {
		return snapshotGate
	}
	state := combinedSourceState(collected.sources, "snapshot", "active_attempt", "terminal_attempt")
	gate := Gate{State: GateUnknown, SourceState: state, Failures: []string{}, Running: []string{}}
	if state == SourceUnavailable || state == SourceCorrupt {
		gate.State = GateUnavailable
	}
	return gate
}

func gateIsUsable(gate Gate) bool {
	return gate.State == GatePending || gate.State == GateFailed || gate.State == GatePassed
}

func gateFromSnapshot(collected collectedEvidence) (Gate, bool) {
	if len(collected.snapshotIssues) == 0 {
		return Gate{}, false
	}
	issue := collected.snapshotIssues[0].issue
	observedAt := collected.snapshot.Snapshot.GeneratedAt.UTC()
	gate := Gate{
		State:       GateUnknown,
		SourceState: collected.snapshot.State,
		EvidenceID:  snapshotEvidenceID(issue.ProjectID, collected.snapshot.Snapshot),
		Source:      "snapshot",
		ObservedAt:  timePointer(observedAt),
		Failures:    []string{},
		Running:     []string{},
	}
	if issue.PullRequest == nil {
		if collected.snapshot.State == SourceLive || collected.snapshot.State == SourceLastKnown {
			gate.State = GateNotApplicable
		}
		return gate, true
	}
	pr := issue.PullRequest
	gate.EvidenceID = pullRequestEvidenceID(issue.ProjectID, int64(pr.Number))
	for _, check := range pr.RequiredCheckFailures {
		if name := strings.TrimSpace(check.Name); name != "" {
			gate.Failures = append(gate.Failures, name)
		}
	}
	gate.Running = append(gate.Running, pr.RunningChecks...)
	if strings.TrimSpace(pr.HydrationUnavailableReason) != "" || strings.TrimSpace(pr.HydrationDegradedReason) != "" {
		gate.State = GateUnavailable
		return gate, true
	}
	if len(gate.Failures) > 0 {
		gate.State = GateFailed
		return gate, true
	}
	gate.State = normalizedGateState(pr.CIStatus)
	if gate.State == GateUnknown && (issue.GatePending || len(gate.Running) > 0) {
		gate.State = GatePending
	}
	return gate, true
}

func gateFromAttempt(attempt *store.WorkAttempt, selection string) (Gate, bool) {
	if attempt == nil || (attempt.PRNumber == nil && strings.TrimSpace(attempt.CIState) == "") {
		return Gate{}, false
	}
	at := attempt.CompletedAt
	if selection == "active" {
		at = attempt.HeartbeatAt
		if at.IsZero() {
			at = attempt.StartedAt
		}
	}
	evidenceID := fmt.Sprintf("attempt:%d", attempt.ID)
	if attempt.PRNumber != nil && *attempt.PRNumber > 0 {
		evidenceID = pullRequestEvidenceID(attempt.ProjectID, *attempt.PRNumber)
	}
	return Gate{
		State:       normalizedGateState(attempt.CIState),
		SourceState: SourceAvailable,
		EvidenceID:  evidenceID,
		Source:      "attempt",
		ObservedAt:  timePointer(at.UTC()),
		Failures:    []string{},
		Running:     []string{},
	}, true
}

func normalizedGateState(value string) GateState {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "success", "succeeded", "passed", "pass":
		return GatePassed
	case "failure", "failed", "error", "cancelled", "canceled", "timed_out":
		return GateFailed
	case "pending", "queued", "in_progress", "running", "expected", "waiting":
		return GatePending
	default:
		return GateUnknown
	}
}

func explanationEvidence(explanation IssueExplanation, collected collectedEvidence) []EvidenceReference {
	collector := map[string]EvidenceReference{}
	add := func(reference EvidenceReference) {
		if reference.ID == "" {
			return
		}
		collector[reference.ID] = reference
	}
	if len(collected.snapshotIssues) > 0 {
		observedAt := collected.snapshot.Snapshot.GeneratedAt.UTC()
		add(EvidenceReference{ID: snapshotEvidenceID(explanation.Identity.ProjectID, collected.snapshot.Snapshot), Kind: EvidenceSnapshot, ObservedAt: timePointer(observedAt)})
	}
	if explanation.LatestTransition != nil {
		add(EvidenceReference{ID: explanation.LatestTransition.EvidenceID, Kind: EvidenceWorkflow, ObservedAt: timePointer(explanation.LatestTransition.At)})
	}
	if explanation.Attempt != nil {
		add(EvidenceReference{ID: explanation.Attempt.EvidenceID, Kind: EvidenceAttempt, ObservedAt: timePointer(explanation.Attempt.StartedAt)})
	}
	if explanation.Eligibility.Latest != nil {
		add(eligibilityEvidence(*explanation.Eligibility.Latest))
	}
	for _, refusal := range explanation.Eligibility.Refusals {
		add(eligibilityEvidence(refusal))
	}
	if explanation.Sessions.Detent != nil {
		add(EvidenceReference{ID: explanation.Sessions.Detent.EvidenceID, Kind: EvidenceSession, ObservedAt: explanation.Sessions.Detent.CompletedAt})
	}
	if explanation.Sessions.Provider != nil {
		add(EvidenceReference{ID: explanation.Sessions.Provider.EvidenceID, Kind: EvidenceProvider, ObservedAt: explanation.Sessions.Provider.CompletedAt})
	}
	if explanation.PullRequest != nil {
		add(EvidenceReference{ID: explanation.PullRequest.EvidenceID, Kind: EvidencePullRequest, ObservedAt: explanation.PullRequest.ObservedAt})
	}
	evidence := make([]EvidenceReference, 0, len(collector))
	for _, reference := range collector {
		evidence = append(evidence, reference)
	}
	slices.SortFunc(evidence, func(left EvidenceReference, right EvidenceReference) int {
		if left.Kind != right.Kind {
			return strings.Compare(string(left.Kind), string(right.Kind))
		}
		return strings.Compare(left.ID, right.ID)
	})
	return evidence
}

func eligibilityEvidence(decision EligibilityDecision) EvidenceReference {
	kind := EvidenceScheduler
	if decision.Source == "admission" {
		kind = EvidenceAdmission
	}
	return EvidenceReference{ID: decision.EvidenceID, Kind: kind, ObservedAt: timePointer(decision.At)}
}

func sourceState(sources []SourceStatus, name string) SourceState {
	for _, source := range sources {
		if source.Name == name {
			return source.State
		}
	}
	return SourceUnavailable
}

func combinedSourceState(sources []SourceStatus, names ...string) SourceState {
	state := SourceUnavailable
	for _, name := range names {
		candidate := sourceState(sources, name)
		switch candidate {
		case SourceAvailable, SourceLive:
			return SourceAvailable
		case SourceLastKnown:
			if state != SourceAvailable {
				state = SourceLastKnown
			}
		case SourceExpired:
			if state == SourceUnavailable || state == SourceCorrupt {
				state = SourceExpired
			}
		case SourceCorrupt:
			if state == SourceUnavailable {
				state = SourceCorrupt
			}
		}
	}
	return state
}

func snapshotEvidenceID(projectID string, snapshot telemetry.Snapshot) string {
	if snapshot.GeneratedAt.IsZero() {
		return ""
	}
	if snapshot.Seq > 0 {
		return fmt.Sprintf("snapshot:%s:%d", strings.TrimSpace(projectID), snapshot.Seq)
	}
	return fmt.Sprintf("snapshot:%s:%d", strings.TrimSpace(projectID), snapshot.GeneratedAt.UTC().UnixNano())
}

func pullRequestEvidenceID(projectID string, number int64) string {
	if number <= 0 {
		return ""
	}
	return fmt.Sprintf("pull-request:%s:%d", strings.TrimSpace(projectID), number)
}

func providerEvidenceID(backend string, id string) string {
	backend = strings.ToLower(strings.TrimSpace(backend))
	if backend == "" {
		backend = "unknown"
	}
	return "provider-session:" + backend + ":" + strings.TrimSpace(id)
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func issueObservationTime(issue telemetry.Issue) time.Time {
	for _, value := range []*time.Time{issue.UpdatedAt, issue.StageUpdatedAt, issue.CurrentLaneEnteredAt, issue.CreatedAt} {
		if value != nil {
			return value.UTC()
		}
	}
	return time.Time{}
}

func issueIdentityKey(issue telemetry.Issue) string {
	return strings.Join([]string{strings.TrimSpace(issue.ID), strings.TrimSpace(issue.Identifier), strings.TrimSpace(issue.URL)}, "\x00")
}
