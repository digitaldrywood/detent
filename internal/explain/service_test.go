package explain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestServiceRequiresScopedIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query Query
		want  error
	}{
		{name: "missing project", query: Query{IssueID: "issue-1"}, want: ErrProjectRequired},
		{name: "missing issue", query: Query{ProjectID: "detent"}, want: ErrIssueReferenceNeeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Dependencies{}).Explain(context.Background(), tt.query)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Explain() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestServiceKeepsProjectsIsolated(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	reader := &evidenceReader{
		observation: SnapshotObservation{
			State: SourceLive,
			Snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				BoardIssues: []telemetry.Issue{
					{ID: "same-id", Identifier: "owner/a#1", ProjectID: "project-a", State: "Todo", Title: "A"},
					{ID: "same-id", Identifier: "owner/b#1", ProjectID: "project-b", State: "Blocked", Title: "B"},
				},
			},
		},
		workflow: []store.WorkflowPhaseEvent{
			{ID: 1, ProjectID: "project-a", IssueID: "same-id", Identifier: "owner/a#1", PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Todo", Status: "entered", StartedAt: now.Add(-time.Minute), MetadataJSON: `{"provenance":{"origin":"human"}}`},
			{ID: 2, ProjectID: "project-b", IssueID: "same-id", Identifier: "owner/b#1", PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Blocked", Status: "entered", StartedAt: now, MetadataJSON: `{"provenance":{"origin":"routine"}}`},
		},
		active: []store.WorkAttempt{
			{ID: 11, ProjectID: "project-a", IssueID: "same-id", Identifier: "owner/a#1", Status: store.WorkAttemptStatusActive, StartedAt: now},
			{ID: 12, ProjectID: "project-b", IssueID: "same-id", Identifier: "owner/b#1", Status: store.WorkAttemptStatusActive, StartedAt: now.Add(time.Minute)},
		},
		decisions: []store.SchedulerDecision{
			{ID: 21, ProjectID: "project-a", IssueID: "same-id", Identifier: "owner/a#1", Result: store.SchedulerDecisionResultSelected, Selected: true, DecisionAt: now},
			{ID: 22, ProjectID: "project-b", IssueID: "same-id", Identifier: "owner/b#1", Result: store.SchedulerDecisionResultSkipped, DecisionAt: now.Add(time.Minute)},
		},
		session: &store.IssueAgentSession{ProjectID: "project-b", DetentSessionID: 99, ProviderSessionID: "wrong-project", CompletedAt: now},
		proposals: []admissionmodel.Proposal{
			{ID: "a", ProjectID: "project-a", IssueID: "same-id", Status: admissionmodel.ProposalAccepted, ResolvedAt: now},
			{ID: "b", ProjectID: "project-b", IssueID: "same-id", Status: admissionmodel.ProposalRejected, ResolvedAt: now.Add(time.Minute)},
		},
	}

	got, err := newTestService(now, reader).Explain(context.Background(), Query{ProjectID: "project-a", IssueID: "same-id"})
	if err != nil {
		t.Fatalf("Explain() error = %v", err)
	}
	if got.Identity.Identifier != "owner/a#1" || got.Identity.Title != "A" || got.CurrentLane.Name != "Todo" {
		t.Fatalf("identity/lane = %#v / %#v, want project-a", got.Identity, got.CurrentLane)
	}
	if got.LatestTransition == nil || got.LatestTransition.EvidenceID != "workflow:1" {
		t.Fatalf("latest transition = %#v, want workflow:1", got.LatestTransition)
	}
	if got.Attempt == nil || got.Attempt.ID != 11 || got.Eligibility.State != EligibilityEligible {
		t.Fatalf("attempt/eligibility = %#v / %#v, want project-a evidence", got.Attempt, got.Eligibility)
	}
	if got.Sessions.Detent != nil || got.Sessions.Provider != nil {
		t.Fatalf("sessions = %#v, want cross-project session ignored", got.Sessions)
	}
	for _, evidence := range got.Evidence {
		if strings.Contains(evidence.ID, "wrong-project") || evidence.ID == "workflow:2" || evidence.ID == "attempt:12" || evidence.ID == "scheduler:22" {
			t.Fatalf("cross-project evidence leaked: %#v", got.Evidence)
		}
	}
}

func TestServiceNormalizesDecoratedIssueURLs(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	canonicalURL := "https://github.com/digitaldrywood/detent/issues/1639"
	reader := &evidenceReader{observation: liveIssueObservation(now, telemetry.Issue{
		ID:        "issue-1639",
		URL:       canonicalURL,
		ProjectID: "detent",
		State:     "In Progress",
	})}
	tests := []struct {
		name  string
		query Query
	}{
		{name: "reference", query: Query{ProjectID: "detent", Reference: canonicalURL + "?notification=thread#event"}},
		{name: "issue URL", query: Query{ProjectID: "detent", IssueURL: canonicalURL + "?notification=thread#event"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := newTestService(now, reader).Explain(context.Background(), tt.query)
			if err != nil {
				t.Fatalf("Explain() error = %v", err)
			}
			if !got.Found || got.Identity.IssueURL != canonicalURL {
				t.Fatalf("explanation = %#v, want canonical URL %q", got, canonicalURL)
			}
		})
	}
}

func TestServiceSeparatesCurrentLaneFromEnteredTransition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	transitionAt := now.Add(-time.Minute)
	metadata := provenance.Apply(`{"private":"never-return"}`, provenance.Attribution{
		Origin: provenance.OriginHuman,
		Actor:  &provenance.Actor{Login: "ada", Kind: "User"},
	}, &provenance.Admission{ProposalID: "proposal-1", Attributed: true})
	reader := &evidenceReader{
		observation: liveIssueObservation(now, telemetry.Issue{ID: "issue-1", Identifier: "owner/repo#1", ProjectID: "detent", State: "Rework"}),
		workflow: []store.WorkflowPhaseEvent{
			{ID: 40, ProjectID: "detent", IssueID: "issue-1", PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Merging", Status: "exited", StartedAt: now.Add(-time.Hour), FinishedAt: transitionAt, MetadataJSON: metadata},
			{ID: 41, ProjectID: "detent", IssueID: "issue-1", PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Blocked", PreviousPhaseName: "Merging", Reason: "required_checks_missing", Status: "entered", StartedAt: transitionAt, EndpointFamily: "tracker", MetadataJSON: metadata},
		},
	}

	got, err := newTestService(now, reader).Explain(context.Background(), Query{ProjectID: "detent", IssueID: "issue-1"})
	if err != nil {
		t.Fatalf("Explain() error = %v", err)
	}
	if got.CurrentLane.Name != "Rework" || got.CurrentLane.Freshness != SourceLive || got.CurrentLane.Degraded {
		t.Fatalf("current lane = %#v, want live Rework snapshot", got.CurrentLane)
	}
	if got.LatestTransition == nil || got.LatestTransition.EvidenceID != "workflow:41" || got.LatestTransition.To != "Blocked" || got.LatestTransition.From != "Merging" {
		t.Fatalf("latest transition = %#v, want entered workflow:41", got.LatestTransition)
	}
	if got.LatestTransition.Actor == nil || got.LatestTransition.Actor.Login != "ada" ||
		got.LatestTransition.Provenance.Origin != "human" ||
		got.LatestTransition.Provenance.Initiator != string(provenance.InitiatorHuman) ||
		!got.LatestTransition.Provenance.Trustworthy ||
		got.LatestTransition.Provenance.TrustworthySince == nil ||
		got.LatestTransition.Provenance.Admission == nil {
		t.Fatalf("parsed provenance = %#v", got.LatestTransition)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(raw), "never-return") || strings.Contains(string(raw), "metadata_json") {
		t.Fatalf("IssueExplanation leaked raw metadata: %s", raw)
	}
}

func TestServiceDistinguishesSnapshotFreshness(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	issue := telemetry.Issue{ID: "issue-1", ProjectID: "detent", State: "Rework"}
	workflow := []store.WorkflowPhaseEvent{{ID: 1, ProjectID: "detent", IssueID: "issue-1", PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Todo", Status: "entered", StartedAt: now.Add(-time.Hour), MetadataJSON: `{"provenance":{"origin":"unknown"}}`}}
	tests := []struct {
		name        string
		observation SnapshotObservation
		wantState   SourceState
		wantLane    string
	}{
		{name: "live", observation: liveIssueObservation(now, issue), wantState: SourceLive, wantLane: "Rework"},
		{name: "last known", observation: SnapshotObservation{State: SourceLastKnown, Snapshot: telemetry.Snapshot{GeneratedAt: now.Add(-time.Minute), BoardIssues: []telemetry.Issue{issue}}}, wantState: SourceLastKnown, wantLane: "Rework"},
		{name: "expired", observation: SnapshotObservation{State: SourceLastKnown, ExpiresAt: timePointer(now.Add(-time.Second)), Snapshot: telemetry.Snapshot{GeneratedAt: now.Add(-time.Hour), BoardIssues: []telemetry.Issue{issue}}}, wantState: SourceExpired, wantLane: "Rework"},
		{name: "unavailable", observation: SnapshotObservation{State: SourceUnavailable, Snapshot: telemetry.Snapshot{GeneratedAt: now, BoardIssues: []telemetry.Issue{issue}}}, wantState: SourceUnavailable},
		{name: "corrupt", observation: SnapshotObservation{State: SourceCorrupt, Snapshot: telemetry.Snapshot{GeneratedAt: now, BoardIssues: []telemetry.Issue{issue}}}, wantState: SourceCorrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &evidenceReader{observation: tt.observation, workflow: workflow}
			got, err := newTestService(now, reader).Explain(context.Background(), Query{ProjectID: "detent", IssueID: "issue-1"})
			if err != nil {
				t.Fatalf("Explain() error = %v", err)
			}
			if got.CurrentLane.Freshness != tt.wantState || got.CurrentLane.Name != tt.wantLane {
				t.Fatalf("current lane = %#v, want state %q lane %q", got.CurrentLane, tt.wantState, tt.wantLane)
			}
			if tt.wantState != SourceLive && !got.CurrentLane.Degraded {
				t.Fatalf("current lane degraded = false for %q", tt.wantState)
			}
		})
	}
}

func TestServiceReportsOnlyRecordedEligibility(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		decisions []store.SchedulerDecision
		proposals []admissionmodel.Proposal
		want      EligibilityState
		refusals  int
	}{
		{name: "no recorded decision", want: EligibilityUnknown},
		{name: "scheduler refusal", decisions: []store.SchedulerDecision{{ID: 2, ProjectID: "detent", IssueID: "issue-1", Result: store.SchedulerDecisionResultSkipped, Reason: "capacity", DecisionAt: now}}, want: EligibilityRefused, refusals: 1},
		{name: "scheduler selection", decisions: []store.SchedulerDecision{{ID: 3, ProjectID: "detent", IssueID: "issue-1", Result: store.SchedulerDecisionResultSelected, Selected: true, DecisionAt: now}}, want: EligibilityEligible},
		{name: "admission rejection", proposals: []admissionmodel.Proposal{{ID: "proposal-1", ProjectID: "detent", IssueID: "issue-1", Status: admissionmodel.ProposalRejected, ResolvedAt: now}}, want: EligibilityRefused, refusals: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &evidenceReader{
				observation: liveIssueObservation(now, telemetry.Issue{ID: "issue-1", ProjectID: "detent", State: "Todo"}),
				decisions:   tt.decisions,
				proposals:   tt.proposals,
			}
			got, err := newTestService(now, reader).Explain(context.Background(), Query{ProjectID: "detent", IssueID: "issue-1"})
			if err != nil {
				t.Fatalf("Explain() error = %v", err)
			}
			if got.Eligibility.State != tt.want || len(got.Eligibility.Refusals) != tt.refusals {
				t.Fatalf("eligibility = %#v, want state %q refusals %d", got.Eligibility, tt.want, tt.refusals)
			}
		})
	}
}

func TestServiceAttemptSessionAndGatePrecedence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	prNumber := int64(11)
	reader := &evidenceReader{
		observation: SnapshotObservation{
			State: SourceLastKnown,
			Snapshot: telemetry.Snapshot{GeneratedAt: now.Add(-time.Minute), BoardIssues: []telemetry.Issue{{
				ID: "issue-1", ProjectID: "detent", State: "Merging", PullRequest: &telemetry.PullRequest{Number: 10, CIStatus: "pending"},
			}}},
		},
		active: []store.WorkAttempt{
			{ID: 4, ProjectID: "detent", IssueID: "issue-1", Status: store.WorkAttemptStatusActive, StartedAt: now.Add(-2 * time.Minute)},
			{ID: 5, ProjectID: "detent", IssueID: "issue-1", PRNumber: &prNumber, WorkerType: "codex", Status: store.WorkAttemptStatusActive, StartedAt: now.Add(-time.Minute), HeartbeatAt: now, CIState: "passed", DetentSessionID: 50, ProviderSessionID: "provider-active"},
		},
		terminal: []store.WorkAttempt{{ID: 6, ProjectID: "detent", IssueID: "issue-1", Status: store.WorkAttemptStatusTerminal, CompletedAt: now.Add(time.Minute), DetentSessionID: 60}},
		session:  &store.IssueAgentSession{ProjectID: "detent", DetentSessionID: 70, ProviderSessionID: "provider-completed", AgentBackendKind: "codex", CompletedAt: now.Add(2 * time.Minute)},
	}

	got, err := newTestService(now, reader).Explain(context.Background(), Query{ProjectID: "detent", IssueID: "issue-1"})
	if err != nil {
		t.Fatalf("Explain() error = %v", err)
	}
	if got.Attempt == nil || got.Attempt.ID != 5 || got.Attempt.Selection != "active" {
		t.Fatalf("attempt = %#v, want newest active attempt 5", got.Attempt)
	}
	if got.Sessions.Detent == nil || got.Sessions.Detent.ID != "50" || got.Sessions.Provider == nil || got.Sessions.Provider.ID != "provider-active" {
		t.Fatalf("sessions = %#v, want active attempt pointers", got.Sessions)
	}
	if got.PullRequest == nil || got.PullRequest.Number != 11 || got.PullRequest.Source != "attempt" {
		t.Fatalf("pull request = %#v, want active attempt PR 11", got.PullRequest)
	}
	if got.RequiredGate.State != GatePassed || got.RequiredGate.Source != "attempt" {
		t.Fatalf("required gate = %#v, want active attempt passed", got.RequiredGate)
	}
}

func TestServiceActiveAttemptGateFallback(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	prNumber := int64(11)
	tests := []struct {
		name       string
		issue      telemetry.Issue
		wantState  GateState
		wantSource string
	}{
		{name: "snapshot has no pull request", issue: telemetry.Issue{}, wantState: GatePassed, wantSource: "attempt"},
		{name: "snapshot hydration unavailable", issue: telemetry.Issue{PullRequest: &telemetry.PullRequest{Number: 11, HydrationUnavailableReason: "rate_limit"}}, wantState: GatePassed, wantSource: "attempt"},
		{name: "snapshot gate unknown", issue: telemetry.Issue{PullRequest: &telemetry.PullRequest{Number: 11}}, wantState: GatePassed, wantSource: "attempt"},
		{name: "snapshot gate is usable", issue: telemetry.Issue{PullRequest: &telemetry.PullRequest{Number: 11, CIStatus: "pending"}}, wantState: GatePending, wantSource: "snapshot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := tt.issue
			issue.ID = "issue-1"
			issue.ProjectID = "detent"
			reader := &evidenceReader{
				observation: liveIssueObservation(now, issue),
				active: []store.WorkAttempt{{
					ID: 5, ProjectID: "detent", IssueID: "issue-1", PRNumber: &prNumber,
					Status: store.WorkAttemptStatusActive, StartedAt: now.Add(-time.Minute), HeartbeatAt: now, CIState: "passed",
				}},
			}
			got, err := newTestService(now, reader).Explain(context.Background(), Query{ProjectID: "detent", IssueID: "issue-1"})
			if err != nil {
				t.Fatalf("Explain() error = %v", err)
			}
			if got.RequiredGate.State != tt.wantState || got.RequiredGate.Source != tt.wantSource {
				t.Fatalf("required gate = %#v, want state %q from %q", got.RequiredGate, tt.wantState, tt.wantSource)
			}
		})
	}
}

func TestServiceDistinguishesRequiredGateStates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		issue telemetry.Issue
		want  GateState
	}{
		{name: "unknown", issue: telemetry.Issue{PullRequest: &telemetry.PullRequest{Number: 1}}, want: GateUnknown},
		{name: "unavailable", issue: telemetry.Issue{PullRequest: &telemetry.PullRequest{Number: 1, HydrationUnavailableReason: "rate_limit"}}, want: GateUnavailable},
		{name: "not applicable", issue: telemetry.Issue{}, want: GateNotApplicable},
		{name: "pending", issue: telemetry.Issue{PullRequest: &telemetry.PullRequest{Number: 1, CIStatus: "pending"}}, want: GatePending},
		{name: "failed", issue: telemetry.Issue{PullRequest: &telemetry.PullRequest{Number: 1, RequiredCheckFailures: []telemetry.PullRequestCheck{{Name: "test"}}}}, want: GateFailed},
		{name: "passed", issue: telemetry.Issue{PullRequest: &telemetry.PullRequest{Number: 1, CIStatus: "success"}}, want: GatePassed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := tt.issue
			issue.ID = "issue-1"
			issue.ProjectID = "detent"
			reader := &evidenceReader{observation: liveIssueObservation(now, issue)}
			got, err := newTestService(now, reader).Explain(context.Background(), Query{ProjectID: "detent", IssueID: "issue-1"})
			if err != nil {
				t.Fatalf("Explain() error = %v", err)
			}
			if got.RequiredGate.State != tt.want {
				t.Fatalf("required gate = %#v, want %q", got.RequiredGate, tt.want)
			}
		})
	}
}

func TestServiceDegradesOnlyFailedSection(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	reader := &evidenceReader{
		observation: liveIssueObservation(now, telemetry.Issue{ID: "issue-1", ProjectID: "detent", State: "Rework"}),
		workflowErr: errors.New("database unavailable"),
	}
	got, err := newTestService(now, reader).Explain(context.Background(), Query{ProjectID: "detent", IssueID: "issue-1"})
	if err != nil {
		t.Fatalf("Explain() error = %v", err)
	}
	if !got.Found || got.CurrentLane.Name != "Rework" || got.LatestTransition != nil {
		t.Fatalf("partial explanation = %#v, want known lane with unavailable workflow", got)
	}
	status := findSourceStatus(got.Sources, "workflow")
	if status.State != SourceUnavailable || status.Code != "read_failed" {
		t.Fatalf("workflow status = %#v", status)
	}
}

func TestServiceMarksMalformedProvenanceUnknown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	reader := &evidenceReader{
		observation: liveIssueObservation(now, telemetry.Issue{ID: "issue-1", ProjectID: "detent", State: "Rework"}),
		workflow:    []store.WorkflowPhaseEvent{{ID: 7, ProjectID: "detent", IssueID: "issue-1", PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Rework", Status: "entered", StartedAt: now, MetadataJSON: "not-json"}},
	}
	got, err := newTestService(now, reader).Explain(context.Background(), Query{ProjectID: "detent", IssueID: "issue-1"})
	if err != nil {
		t.Fatalf("Explain() error = %v", err)
	}
	if got.LatestTransition == nil || got.LatestTransition.Provenance.State != SourceCorrupt || got.LatestTransition.Provenance.Origin != "indeterminate" || got.LatestTransition.Actor != nil {
		t.Fatalf("transition provenance = %#v", got.LatestTransition)
	}
}

func TestServiceMarksOnlyPostBoundarySchemaAttributionTrustworthy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 12, 18, 0, 0, 0, time.UTC)
	boundary := now.Add(-time.Hour)
	trusted := provenance.Apply("{}", provenance.AttributionFromSource(provenance.SourceDetentAgentSession, provenance.Actor{Login: "worker", Kind: "User"}), nil)
	tests := []struct {
		name       string
		at         time.Time
		metadata   string
		wantOrigin string
		wantTrust  bool
	}{
		{name: "legacy human row remains untrusted", at: now, metadata: `{"provenance":{"origin":"human","actor":{"login":"corylanou","kind":"User"}}}`, wantOrigin: "human"},
		{name: "backdated schema row remains untrusted", at: boundary.Add(-time.Minute), metadata: trusted, wantOrigin: "agent"},
		{name: "post-boundary schema row is trusted", at: boundary.Add(time.Minute), metadata: trusted, wantOrigin: "agent", wantTrust: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &evidenceReader{
				observation:        liveIssueObservation(now, telemetry.Issue{ID: "issue-1", ProjectID: "detent", State: "Blocked"}),
				workflow:           []store.WorkflowPhaseEvent{{ID: 1, ProjectID: "detent", IssueID: "issue-1", PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Blocked", Status: "entered", StartedAt: tt.at, MetadataJSON: tt.metadata}},
				provenanceBoundary: boundary,
			}
			got, err := newTestService(now, reader).Explain(t.Context(), Query{ProjectID: "detent", IssueID: "issue-1"})
			if err != nil {
				t.Fatalf("Explain() error = %v", err)
			}
			if got.LatestTransition == nil || got.LatestTransition.Provenance.Origin != tt.wantOrigin || got.LatestTransition.Provenance.Trustworthy != tt.wantTrust {
				t.Fatalf("provenance = %#v, want origin %q trustworthy %t", got.LatestTransition, tt.wantOrigin, tt.wantTrust)
			}
		})
	}
}

func TestServiceRejectsAmbiguousIssueIdentity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	reader := &evidenceReader{observation: SnapshotObservation{State: SourceLive, Snapshot: telemetry.Snapshot{
		GeneratedAt: now,
		BoardIssues: []telemetry.Issue{
			{ID: "issue-a", Identifier: "owner/repo#1", Number: 1, ProjectID: "detent"},
			{ID: "issue-b", Identifier: "owner/repo-renamed#1", Number: 1, ProjectID: "detent"},
		},
	}}}
	_, err := newTestService(now, reader).Explain(context.Background(), Query{ProjectID: "detent", Reference: "#1"})
	var ambiguous *AmbiguousIdentityError
	if !errors.As(err, &ambiguous) || ambiguous.Field != "issue_id" || !reflect.DeepEqual(ambiguous.Values, []string{"issue-a", "issue-b"}) {
		t.Fatalf("Explain() error = %#v, want deterministic issue_id ambiguity", err)
	}
}

func TestServiceEvidenceIDsAreDeterministicAndNamespaced(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	reader := &evidenceReader{
		observation: liveIssueObservation(now, telemetry.Issue{ID: "issue-1", ProjectID: "detent", State: "Todo", PullRequest: &telemetry.PullRequest{Number: 42}}),
		workflow:    []store.WorkflowPhaseEvent{{ID: 8, ProjectID: "detent", IssueID: "issue-1", PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Todo", Status: "entered", StartedAt: now, MetadataJSON: `{"provenance":{"origin":"routine"}}`}},
		decisions:   []store.SchedulerDecision{{ID: 9, ProjectID: "detent", IssueID: "issue-1", Result: store.SchedulerDecisionResultSkipped, DecisionAt: now}},
	}
	service := newTestService(now, reader)
	first, err := service.Explain(context.Background(), Query{ProjectID: "detent", IssueID: "issue-1"})
	if err != nil {
		t.Fatalf("Explain(first) error = %v", err)
	}
	second, err := service.Explain(context.Background(), Query{ProjectID: "detent", IssueID: "issue-1"})
	if err != nil {
		t.Fatalf("Explain(second) error = %v", err)
	}
	if !reflect.DeepEqual(first.Evidence, second.Evidence) {
		t.Fatalf("evidence changed: %#v != %#v", first.Evidence, second.Evidence)
	}
	for _, evidence := range first.Evidence {
		if !strings.Contains(evidence.ID, ":") {
			t.Fatalf("evidence ID is not namespaced: %q", evidence.ID)
		}
	}
}

type evidenceReader struct {
	observation        SnapshotObservation
	snapshotErr        error
	workflow           []store.WorkflowPhaseEvent
	workflowErr        error
	active             []store.WorkAttempt
	activeErr          error
	terminal           []store.WorkAttempt
	terminalErr        error
	decisions          []store.SchedulerDecision
	decisionsErr       error
	session            *store.IssueAgentSession
	sessionErr         error
	proposals          []admissionmodel.Proposal
	proposalsErr       error
	provenanceBoundary time.Time
	provenanceErr      error
}

func (r *evidenceReader) Snapshot(context.Context) (SnapshotObservation, error) {
	return r.observation, r.snapshotErr
}

func (r *evidenceReader) IssueWorkflowTimeline(context.Context, store.IssueIdentity) (store.WorkflowTimeline, error) {
	return store.WorkflowTimeline{Events: append([]store.WorkflowPhaseEvent(nil), r.workflow...)}, r.workflowErr
}

func (r *evidenceReader) ProvenanceAttributionTrustBoundary(context.Context) (time.Time, error) {
	return r.provenanceBoundary, r.provenanceErr
}

func (r *evidenceReader) ListActiveWorkAttempts(context.Context, store.WorkAttemptQuery) ([]store.WorkAttempt, error) {
	return append([]store.WorkAttempt(nil), r.active...), r.activeErr
}

func (r *evidenceReader) ListRecentTerminalWorkAttempts(context.Context, store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error) {
	return append([]store.WorkAttempt(nil), r.terminal...), r.terminalErr
}

func (r *evidenceReader) ListIssueSchedulerDecisions(context.Context, store.IssueSchedulerDecisionQuery) ([]store.SchedulerDecision, error) {
	return append([]store.SchedulerDecision(nil), r.decisions...), r.decisionsErr
}

func (r *evidenceReader) LatestIssueAgentSession(context.Context, store.IssueIdentity) (store.IssueAgentSession, error) {
	if r.sessionErr != nil {
		return store.IssueAgentSession{}, r.sessionErr
	}
	if r.session == nil {
		return store.IssueAgentSession{}, store.ErrNotFound
	}
	return *r.session, nil
}

func (r *evidenceReader) AdmissionProposalHistory(context.Context, string, string) ([]admissionmodel.Proposal, error) {
	return append([]admissionmodel.Proposal(nil), r.proposals...), r.proposalsErr
}

func newTestService(now time.Time, reader *evidenceReader) *Service {
	cloned := *reader
	reader = &cloned
	if reader.provenanceBoundary.IsZero() {
		reader.provenanceBoundary = now.Add(-24 * time.Hour)
	}
	return New(Dependencies{
		Snapshots:  reader,
		Workflow:   reader,
		Provenance: reader,
		Attempts:   reader,
		Scheduler:  reader,
		Sessions:   reader,
		Admission:  reader,
		Now:        func() time.Time { return now },
	})
}

func liveIssueObservation(at time.Time, issue telemetry.Issue) SnapshotObservation {
	return SnapshotObservation{State: SourceLive, Snapshot: telemetry.Snapshot{GeneratedAt: at, BoardIssues: []telemetry.Issue{issue}}}
}

func findSourceStatus(statuses []SourceStatus, name string) SourceStatus {
	for _, status := range statuses {
		if status.Name == name {
			return status
		}
	}
	return SourceStatus{}
}
