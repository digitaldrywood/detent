package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/activehours"
	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type releaseErrorGlobalScheduler struct {
	scheduler.GlobalScheduler
	err error
}

func (s *releaseErrorGlobalScheduler) ReleaseSlot(slot scheduler.Slot) error {
	if err := s.GlobalScheduler.ReleaseSlot(slot); err != nil {
		return err
	}
	err := s.err
	s.err = nil
	return err
}

func TestManagerDisabledRegistersNoSchedule(t *testing.T) {
	t.Parallel()

	manager, err := New(Settings{Config: config.BacklogAdmission{Enabled: false}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if manager.Enabled() {
		t.Fatal("Enabled() = true")
	}
	next, scheduled, err := manager.nextScheduled(context.Background())
	if err != nil || scheduled || !next.IsZero() {
		t.Fatalf("nextScheduled() = %v, %t, %v", next, scheduled, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestManagerUpdateProjectCandidate(t *testing.T) {
	t.Parallel()

	manager, err := New(Settings{
		ProjectID: "detent",
		Config:    config.BacklogAdmission{Enabled: false},
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	windows := []string{"Mon-Fri 22:00-06:00"}
	manager.UpdateProjectCandidate(scheduler.ProjectCandidate{
		ID: " alpha ",
		ActiveHours: activehours.Config{
			Timezone: " America/Chicago ",
			Windows:  windows,
		},
	})
	windows[0] = "Sat-Sun 00:00-24:00"

	manager.mu.RLock()
	got := cloneSettings(manager.settings).ProjectCandidate
	manager.mu.RUnlock()
	want := scheduler.ProjectCandidate{
		ID: "alpha",
		ActiveHours: activehours.Config{
			Timezone: "America/Chicago",
			Windows:  []string{"Mon-Fri 22:00-06:00"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProjectCandidate = %#v, want %#v", got, want)
	}

	manager.UpdateProjectCandidate(scheduler.ProjectCandidate{})
	manager.mu.RLock()
	got = manager.settings.ProjectCandidate
	manager.mu.RUnlock()
	if got.ID != "detent" {
		t.Fatalf("ProjectCandidate.ID = %q, want detent", got.ID)
	}

	var missing *Manager
	missing.UpdateProjectCandidate(scheduler.ProjectCandidate{})
}

func TestManagerEnforcesOrderingAndAllCapsBeforeWritingComments(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	priorityOne, priorityTwo, priorityThree := 1, 2, 3
	issues := []connector.Issue{
		admissionIssueFixture("issue-3", "DD-3", priorityThree, now.Add(-3*time.Hour)),
		admissionIssueFixture("issue-1", "DD-1", priorityOne, now.Add(-time.Hour)),
		admissionIssueFixture("issue-2", "DD-2", priorityTwo, now.Add(-2*time.Hour)),
	}
	tracker := memory.New(memory.Config{Issues: issues, Stateful: true, Now: func() time.Time { return now }})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.MaxCandidatesPerRun = 2
	settings.Config.MaxProposalsPerRun = 1
	settings.Config.MaxOpenProposals = 1
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if got, want := agent.candidateIDs[0], []string{"issue-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate order = %#v, want %#v", got, want)
	}
	if result.CandidatesFound != 3 || result.Candidates != 1 || len(result.Proposals) != 1 ||
		result.Truncated["candidates"] != 2 {
		t.Fatalf("result = %#v", result)
	}
	open, err := backend.OpenAdmissionProposals(context.Background(), "detent", 0)
	if err != nil {
		t.Fatalf("OpenAdmissionProposals() error = %v", err)
	}
	if len(open) != 1 || open[0].IssueID != "issue-1" || open[0].CommentedAt.IsZero() {
		t.Fatalf("open proposals = %#v", open)
	}
	events := tracker.Events()
	commentCount := 0
	for _, event := range events {
		if event.Kind == memory.EventKindComment {
			commentCount++
		}
		if event.Kind == memory.EventKindStateUpdate {
			t.Fatalf("admission changed issue status: %#v", event)
		}
	}
	if commentCount != 1 {
		t.Fatalf("comment count = %d, want 1", commentCount)
	}

	second, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if agent.calls != 1 || second.Skipped["open_proposal_cap"] == 0 {
		t.Fatalf("second result = %#v, runner calls = %d, want pre-agent open cap", second, agent.calls)
	}
}

func TestManagerDeclinesNonDeliverableCandidatesBeforeRunningAgent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	issues := []connector.Issue{
		admissionIssueFixture("tracker", "PA-10", 1, now),
		admissionIssueFixture("intake", "PA-1728", 1, now.Add(time.Minute)),
		admissionIssueFixture("checklist", "PA-1817", 1, now.Add(2*time.Minute)),
		admissionIssueFixture("optout", "PA-1818", 1, now.Add(3*time.Minute)),
		admissionIssueFixture("actionable", "PA-1819", 1, now.Add(4*time.Minute)),
	}
	issues[0].Title = "PyroApex Platform Architecture — Master Tracker"
	issues[0].Description = "Coordinates the platform work across linked issues."
	issues[1].Title = "POS wave 1 field-test findings — Creswood + Jurassic (intake)"
	issues[1].Description = "Collect findings here before creating implementation issues."
	issues[2].Title = "POS follow-up work"
	issues[2].Description = "- [ ] #1701\n- [ ] #1702\n- [ ] digitaldrywood/pyroapex#1703"
	issues[3].Description = "<!-- detent:no-dispatch -->\nOperator-owned planning record."

	tracker := memory.New(memory.Config{Issues: issues, Stateful: true, Now: func() time.Time { return now }})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.MaxProposalsPerRun = 10
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if got, want := agent.candidateIDs, [][]string{{"actionable"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agent candidates = %#v, want %#v", got, want)
	}
	if result.Skipped["non_deliverable"] != 4 || len(result.Proposals) != 1 {
		t.Fatalf("result = %#v, want four declines and one proposal", result)
	}
	second, err := manager.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if agent.calls != 1 || second.Skipped["non_deliverable"] != 4 {
		t.Fatalf("second result = %#v, runner calls = %d, want persisted declines", second, agent.calls)
	}
	for _, issue := range issues[:4] {
		comments, err := tracker.FetchIssueComments(t.Context(), issue)
		if err != nil {
			t.Fatalf("FetchIssueComments(%s) error = %v", issue.Identifier, err)
		}
		if len(comments) != 1 || !strings.Contains(comments[0].Body, "Detent backlog admission declined") {
			t.Fatalf("comments for %s = %#v, want one decline explanation", issue.Identifier, comments)
		}
	}
}

func TestManagerAppliesCandidateCapBeforeDeclineSideEffects(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	issues := make([]connector.Issue, 4)
	for index := range issues {
		issues[index] = admissionIssueFixture(
			"tracker-"+strconv.Itoa(index),
			"PA-"+strconv.Itoa(index),
			index+1,
			now.Add(time.Duration(index)*time.Minute),
		)
		issues[index].Title += " — Tracker"
	}
	tracker := memory.New(memory.Config{Issues: issues, Stateful: true, Now: func() time.Time { return now }})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.MaxCandidatesPerRun = 2
	settings.Config.MaxProposalsPerRun = 10
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	tests := []struct {
		name          string
		wantDeclines  int
		wantTruncated int
	}{
		{name: "first pass", wantDeclines: 2, wantTruncated: 2},
		{name: "second pass", wantDeclines: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := manager.RunOnce(t.Context())
			if err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if result.Skipped["non_deliverable"] != len(issues) || result.Truncated["candidates"] != tt.wantTruncated {
				t.Fatalf("result = %#v, want %d truncated candidates", result, tt.wantTruncated)
			}
			declines := 0
			comments := 0
			for _, issue := range issues {
				_, found, err := backend.AdmissionDecline(t.Context(), settings.ProjectID, issue.ID, issueFingerprint(issue))
				if err != nil {
					t.Fatalf("AdmissionDecline(%s) error = %v", issue.ID, err)
				}
				if found {
					declines++
				}
				issueComments, err := tracker.FetchIssueComments(t.Context(), issue)
				if err != nil {
					t.Fatalf("FetchIssueComments(%s) error = %v", issue.ID, err)
				}
				comments += len(issueComments)
			}
			if declines != tt.wantDeclines || comments != tt.wantDeclines || agent.calls != 0 {
				t.Fatalf("declines = %d, comments = %d, runner calls = %d, want %d capped declines", declines, comments, agent.calls, tt.wantDeclines)
			}
		})
	}
}

func TestManagerPropagatesCandidateDeclineStoreErrors(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	wantErr := errors.New("decline store failure")
	tests := []struct {
		name      string
		configure func(*faultAdmissionStore)
	}{
		{
			name: "read decline",
			configure: func(backend *faultAdmissionStore) {
				backend.declineReadErr = wantErr
			},
		},
		{
			name: "create decline",
			configure: func(backend *faultAdmissionStore) {
				backend.createDeclineErr = wantErr
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := admissionIssueFixture("tracker", "PA-10", 1, now)
			issue.Title += " — Tracker"
			tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
			backend := &faultAdmissionStore{Store: openManagerTestStore(t)}
			tt.configure(backend)
			manager := &Manager{store: backend}
			settings := admissionTestSettings(tracker, &scriptedAdmissionRunner{})

			_, _, _, err := manager.unproposedCandidates(
				t.Context(),
				settings,
				[]connector.Issue{issue},
				map[string]int{},
				1,
				now,
				1,
			)
			if !errors.Is(err, wantErr) {
				t.Fatalf("unproposedCandidates() error = %v, want %v", err, wantErr)
			}
		})
	}
}

func TestManagerReevaluatesDeclineAfterIssueContentChanges(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "PA-1818", 1, now)
	issue.Description = admissionOptOutMarker + "\nOperator-owned planning record."
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true, Now: func() time.Time { return now }})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return now })

	first, err := manager.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("first RunOnce() error = %v", err)
	}
	if first.Skipped["non_deliverable"] != 1 || agent.calls != 0 {
		t.Fatalf("first result = %#v, runner calls = %d", first, agent.calls)
	}
	if err := tracker.UpdateIssueBody(t.Context(), issue.ID, "## Acceptance criteria\n\n- Implement the bounded fix and add a regression test."); err != nil {
		t.Fatalf("UpdateIssueBody() error = %v", err)
	}
	second, err := manager.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if got, want := agent.candidateIDs, [][]string{{issue.ID}}; !reflect.DeepEqual(got, want) || len(second.Proposals) != 1 {
		t.Fatalf("agent candidates = %#v, result = %#v, want edited issue proposed", got, second)
	}
}

func TestManagerReevaluatesCriteriaDeclineAfterCriteriaChange(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	backend := openManagerTestStore(t)
	declining := staticAdmissionRunner{output: `{"evaluations":[{"issue_id":"issue-1","disposition":"declined","findings":[{"dimension":"Alignment","criterion_quote":"serves a stated current priority","matched":false,"rationale":"The issue does not serve a stated current priority."},{"dimension":"Readiness","criterion_quote":"has an actionable problem statement","matched":false,"rationale":"The issue lacks an actionable problem statement."}],"confidence":0.24}]}`}
	settings := admissionTestSettings(tracker, declining)
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	first, err := manager.RunOnce(t.Context())
	if err != nil || first.Skipped[admissionDeclineCriteriaNotMet] != 1 {
		t.Fatalf("first RunOnce() = %#v, %v", first, err)
	}
	proposing := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings.Runner = proposing
	settings.Criteria.Text += "\nOperator guidance changed."
	if err := manager.Update(settings); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	second, err := manager.RunOnce(t.Context())
	if err != nil || len(second.Proposals) != 1 || proposing.calls != 1 {
		t.Fatalf("second RunOnce() = %#v, %v, runner calls = %d", second, err, proposing.calls)
	}
}

func TestManagerDoesNotDuplicateDeclineCommentAfterStoreMarkFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "PA-1818", 1, now)
	issue.Description = admissionOptOutMarker
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true, Now: func() time.Time { return now }})
	backend := &faultAdmissionStore{
		Store:                 openManagerTestStore(t),
		declineCommentMarkErr: errors.New("mark decline comment"),
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return now })

	if _, err := manager.RunOnce(t.Context()); err == nil || !strings.Contains(err.Error(), "mark decline comment") {
		t.Fatalf("first RunOnce() error = %v, want mark failure", err)
	}
	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	comments, err := tracker.FetchIssueComments(t.Context(), issue)
	if err != nil {
		t.Fatalf("FetchIssueComments() error = %v", err)
	}
	if len(comments) != 1 || agent.calls != 0 {
		t.Fatalf("comments = %#v, runner calls = %d, want one comment and no agent run", comments, agent.calls)
	}
}

func TestManagerDeclineSupersedesAcceptedOpenProposal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "PA-10", 1, now)
	issue.Title = "PyroApex Platform Architecture — Master Tracker"
	issue.Description = "Coordinates platform issues."
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	decisionAt := now.Add(time.Minute)
	issue.Comments = []connector.IssueComment{{
		ID:               "accept-1",
		Body:             admissionAcceptCommand(proposal.ID),
		AuthorLogin:      "operator",
		AuthorKind:       "User",
		AuthorAuthorized: true,
		CreatedAt:        &decisionAt,
	}}
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true, Now: func() time.Time { return now.Add(2 * time.Minute) }})
	backend := openManagerTestStore(t)
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return now.Add(2 * time.Minute) })

	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 || history[0].Status != admissionmodel.ProposalSuperseded || history[0].ResolutionReason != admissionResolutionNonDeliverable {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
	for _, event := range tracker.Events() {
		if event.Kind == memory.EventKindStateUpdate {
			t.Fatalf("declined issue changed state: %#v", event)
		}
	}
}

func TestManagerAdmissionDeclinePersistenceBoundaries(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "PA-10", 1, now)
	classification := admissionDeclineClassification{reason: admissionDeclineTracker, detail: "tracker without a completion contract"}
	base := openManagerTestStore(t)
	backend := &faultAdmissionStore{Store: base}
	manager := &Manager{store: backend, now: func() time.Time { return now }}
	settings := Settings{ProjectID: "detent"}

	first, created, err := manager.createAdmissionDecline(t.Context(), settings, issue, classification, now)
	if err != nil || !created {
		t.Fatalf("first createAdmissionDecline() = %#v, %t, %v", first, created, err)
	}
	second, created, err := manager.createAdmissionDecline(t.Context(), settings, issue, classification, now)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("second createAdmissionDecline() = %#v, %t, %v", second, created, err)
	}

	backend.declineReadErr = errors.New("read decline")
	if _, _, err := manager.createAdmissionDecline(t.Context(), settings, issue, classification, now); err == nil || !strings.Contains(err.Error(), "read decline") {
		t.Fatalf("read-error createAdmissionDecline() error = %v", err)
	}
	backend.declineMissing = true
	if _, _, err := manager.createAdmissionDecline(t.Context(), settings, issue, classification, now); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing createAdmissionDecline() error = %v", err)
	}
	backend.createDeclineErr = errors.New("create decline")
	issue.ID = "issue-2"
	if _, _, err := manager.createAdmissionDecline(t.Context(), settings, issue, classification, now); err == nil || !strings.Contains(err.Error(), "create decline") {
		t.Fatalf("create-error createAdmissionDecline() error = %v", err)
	}

	issuesWithoutReader := &admissionIssueStoreWithoutCommentReader{IssueStore: memory.New(memory.Config{})}
	if _, err := manager.ensureAdmissionDeclineComment(t.Context(), issuesWithoutReader, issue, first, false, true); err == nil || !strings.Contains(err.Error(), "comment reader") {
		t.Fatalf("ensureAdmissionDeclineComment() error = %v, want missing reader", err)
	}
	if comment := admissionDeclineComment(first, ""); !strings.Contains(comment, "current untracked state") {
		t.Fatalf("admissionDeclineComment() = %q, want untracked state", comment)
	}
}

func TestManagerRecordsCandidateReaderTruncation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issues := make([]connector.Issue, connector.DefaultCandidatePageSize+2)
	for index := range issues {
		issues[index] = admissionIssueFixture(
			"issue-"+strconv.Itoa(index),
			"DD-"+strconv.Itoa(index),
			1,
			now.Add(time.Duration(index)*time.Minute),
		)
		issues[index].Labels = []string{"skip"}
	}
	tracker := memory.New(memory.Config{Issues: issues, Stateful: true, Now: func() time.Time { return now }})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.ExcludeLabels = []string{"skip"}
	settings.Config.MaxCandidatesPerRun = 2
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Truncated["candidate_reader"] != 1 ||
		result.CandidatesFound != connector.DefaultCandidatePageSize ||
		result.ItemsRead != len(issues) {
		t.Fatalf("result = %#v, want reader truncation at %d candidates", result, connector.DefaultCandidatePageSize)
	}
	if agent.calls != 0 {
		t.Fatalf("runner calls = %d, want no eligible candidates", agent.calls)
	}
	record, ok, err := backend.LatestAdmissionRun(context.Background(), "detent")
	if err != nil || !ok {
		t.Fatalf("LatestAdmissionRun() = %#v, %t, %v", record, ok, err)
	}
	if record.Truncated["candidate_reader"] != 1 {
		t.Fatalf("run ledger truncation = %#v, want candidate_reader=1", record.Truncated)
	}
}

func TestManagerLogsScheduledAdmissionScan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		result         Result
		wantTruncated  bool
		wantTruncation int
	}{
		{
			name: "exhausted scan",
			result: Result{
				ProjectID: "pyroapex",
				ItemsRead: 425,
				Truncated: map[string]int{},
			},
		},
		{
			name: "bounded scan",
			result: Result{
				ProjectID: "pyroapex",
				ItemsRead: 100,
				Truncated: map[string]int{"candidate_reader": 1},
			},
			wantTruncated:  true,
			wantTruncation: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			manager := &Manager{logger: slog.New(slog.NewJSONHandler(&output, nil))}
			manager.logScheduledCompletion(t.Context(), test.result)

			var record struct {
				Event           string         `json:"event"`
				ItemsRead       int            `json:"items_read"`
				CandidatesFound int            `json:"candidates_found"`
				Truncated       bool           `json:"truncated"`
				Truncations     map[string]int `json:"truncations"`
			}
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatalf("decode log record: %v", err)
			}
			if record.Event != "scheduled_backlog_admission_completed" ||
				record.ItemsRead != test.result.ItemsRead ||
				record.CandidatesFound != test.result.CandidatesFound ||
				record.Truncated != test.wantTruncated ||
				record.Truncations["candidate_reader"] != test.wantTruncation {
				t.Fatalf("log record = %#v", record)
			}
		})
	}
}

func TestManagerExpiresProposalAndReproposesUnchangedIssue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	current := now
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{admissionIssueFixture("issue-1", "DD-1", 1, now)},
		Stateful: true,
		Now:      func() time.Time { return current },
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return current })

	first, err := manager.RunOnce(context.Background())
	if err != nil || len(first.Proposals) != 1 {
		t.Fatalf("first RunOnce() = %#v, %v", first, err)
	}
	current = now.AddDate(0, 0, settings.Config.ProposalExpiryDays+1)
	second, err := manager.RunOnce(context.Background())
	if err != nil || len(second.Proposals) != 1 {
		t.Fatalf("second RunOnce() = %#v, %v", second, err)
	}
	history, err := backend.AdmissionProposalHistory(context.Background(), "detent", "issue-1")
	if err != nil {
		t.Fatalf("AdmissionProposalHistory() error = %v", err)
	}
	if len(history) != 2 || history[0].Status != admissionmodel.ProposalOpen || history[1].Status != admissionmodel.ProposalExpired {
		t.Fatalf("history = %#v", history)
	}
	outcomes, err := backend.AdmissionDownstreamOutcomes(context.Background(), "detent")
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("AdmissionDownstreamOutcomes() = %#v, %v, want expiry not counted as acceptance", outcomes, err)
	}
}

func TestManagerAuditCommentDoesNotDuplicateProposal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{admissionIssueFixture("issue-1", "DD-1", 1, now)},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return now })
	if result, err := manager.RunOnce(context.Background()); err != nil || len(result.Proposals) != 1 {
		t.Fatalf("first RunOnce() = %#v, %v", result, err)
	}
	second, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if agent.calls != 1 || second.Skipped["unchanged_open_proposal"] != 1 {
		t.Fatalf("second result = %#v, runner calls = %d", second, agent.calls)
	}
	history, err := backend.AdmissionProposalHistory(context.Background(), "detent", "issue-1")
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %#v, %v", history, err)
	}
}

func TestManagerReconcilesAgainstStoredProposalTarget(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	issue.State = "Todo"
	transitionAt := now.Add(2 * time.Minute)
	decisionAt := now.Add(time.Minute)
	issue.StageUpdatedAt = &transitionAt
	issue.Comments = []connector.IssueComment{{
		ID:               "decision-1",
		Body:             admissionAcceptCommand("proposal-1"),
		AuthorLogin:      "ada",
		AuthorKind:       "User",
		AuthorAuthorized: true,
		CreatedAt:        &decisionAt,
	}}
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	if created, err := backend.CreateAdmissionProposal(context.Background(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.TargetState = "Ready"
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return transitionAt })

	if _, err := manager.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(context.Background(), "detent", issue.ID)
	if err != nil || len(history) != 1 || history[0].Status != admissionmodel.ProposalAccepted {
		t.Fatalf("history = %#v, %v", history, err)
	}
	if history[0].DecisionSeconds != 60 || !history[0].TransitionAt.Equal(transitionAt) ||
		history[0].DecisionCommentID != "decision-1" {
		t.Fatalf("decision evidence = %#v", history[0])
	}
	if agent.calls != 0 {
		t.Fatalf("runner calls = %d, want 0", agent.calls)
	}
}

func TestManagerReconcilesAcceptanceAfterIssueLeavesTargetState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	current := now
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{issue},
		Stateful: true,
		Now:      func() time.Time { return current },
	})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	if created, err := backend.CreateAdmissionProposal(ctx, proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	decisionAt := now.Add(time.Minute)
	current = decisionAt
	if err := tracker.CreateComment(ctx, issue.ID, admissionAcceptCommand(proposal.ID)); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	targetAt := now.Add(2 * time.Minute)
	current = targetAt
	if err := tracker.UpdateIssueState(ctx, issue.ID, proposal.TargetState); err != nil {
		t.Fatalf("UpdateIssueState(%s) error = %v", proposal.TargetState, err)
	}
	if _, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
		ProjectID:  proposal.ProjectID,
		IssueID:    proposal.IssueID,
		Identifier: proposal.IssueIdentifier,
		IssueURL:   proposal.IssueURL,
		PhaseType:  store.WorkflowPhaseTypeLane,
		PhaseName:  proposal.TargetState,
		Reason:     "tracker_state_observed",
		Status:     "entered",
		StartedAt:  targetAt,
		MetadataJSON: provenance.Apply(
			"{}",
			provenance.Attribution{Origin: provenance.OriginUnknown},
			&provenance.Admission{Attributed: false},
		),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	current = now.Add(3 * time.Minute)
	if err := tracker.UpdateIssueState(ctx, issue.ID, "In Progress"); err != nil {
		t.Fatalf("UpdateIssueState(In Progress) error = %v", err)
	}
	manager := newAdmissionTestManager(
		t,
		admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
		backend,
		func() time.Time { return current },
	)

	if _, err := manager.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(ctx, proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
	if history[0].Status != admissionmodel.ProposalAccepted || !history[0].TransitionAt.Equal(targetAt) {
		t.Fatalf("accepted proposal = %#v", history[0])
	}
	timeline, err := backend.IssueWorkflowTimeline(ctx, store.IssueIdentity{ProjectID: proposal.ProjectID, IssueID: proposal.IssueID})
	if err != nil || len(timeline.Events) != 1 ||
		timeline.Events[0].Reason != "admission_proposal_accepted" {
		t.Fatalf("IssueWorkflowTimeline() = %#v, %v", timeline, err)
	}
}

func TestManagerReconcilesOpenProposalFromIssueState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	transitionAt := now.Add(time.Minute)
	tests := []struct {
		name           string
		state          string
		closed         bool
		transition     bool
		recordedTarget bool
		wantStatus     admissionmodel.ProposalStatus
		wantReason     string
		wantActorLogin string
		wantActorKind  string
	}{
		{
			name:           "target state implicitly accepts",
			state:          "Todo",
			transition:     true,
			wantStatus:     admissionmodel.ProposalAccepted,
			wantReason:     admissionResolutionImplicitAccept,
			wantActorLogin: "ada",
			wantActorKind:  "User",
		},
		{
			name:           "recorded target transition accepts after issue moves onward",
			state:          "In Progress",
			recordedTarget: true,
			wantStatus:     admissionmodel.ProposalAccepted,
			wantReason:     admissionResolutionImplicitAccept,
			wantActorLogin: "grace",
			wantActorKind:  "User",
		},
		{
			name:       "closed issue supersedes",
			state:      "Backlog",
			closed:     true,
			transition: true,
			wantStatus: admissionmodel.ProposalSuperseded,
			wantReason: admissionResolutionIssueClosed,
		},
		{
			name:       "terminal issue supersedes",
			state:      "Done",
			transition: true,
			wantStatus: admissionmodel.ProposalSuperseded,
			wantReason: admissionResolutionTerminalState,
		},
		{
			name:       "unactioned proposal stays open",
			state:      "Backlog",
			wantStatus: admissionmodel.ProposalOpen,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
			proposal := admissionTestProposalForIssue("proposal-1", issue, now)
			issue.State = tt.state
			issue.Closed = tt.closed
			issue.StageUpdatedAt = &transitionAt
			tracker := &transitionAdmissionIssueStore{
				Connector: memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true}),
				transition: connector.IssueStateTransition{
					EnteredAt: transitionAt,
					Actor:     connector.IssueActor{Login: "ada", Kind: "User"},
				},
				found: tt.transition,
			}
			backend := openManagerTestStore(t)
			if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
				t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
			}
			if tt.recordedTarget {
				if _, err := backend.RecordWorkflowPhaseEvent(t.Context(), store.WorkflowPhaseEvent{
					ProjectID:  proposal.ProjectID,
					IssueID:    proposal.IssueID,
					Identifier: proposal.IssueIdentifier,
					IssueURL:   proposal.IssueURL,
					PhaseType:  store.WorkflowPhaseTypeLane,
					PhaseName:  proposal.TargetState,
					Reason:     "tracker_state_observed",
					Status:     "entered",
					StartedAt:  transitionAt,
					MetadataJSON: provenance.Apply("{}", provenance.Attribution{
						Origin: provenance.OriginUnknown,
						Actor: &provenance.Actor{
							Login: "grace",
							Kind:  "User",
						},
					}, nil),
				}); err != nil {
					t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
				}
			}
			settings := admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate})
			settings.TerminalStates = []string{"Done"}
			manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return transitionAt })

			if _, err := manager.RunOnce(t.Context()); err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			history, err := backend.AdmissionProposalHistory(t.Context(), proposal.ProjectID, proposal.IssueID)
			if err != nil || len(history) != 1 {
				t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
			}
			got := history[0]
			if got.Status != tt.wantStatus || got.ResolutionReason != tt.wantReason {
				t.Fatalf("resolved proposal = %#v", got)
			}
			if tt.wantStatus == admissionmodel.ProposalAccepted &&
				(got.DecisionActorLogin != tt.wantActorLogin || got.DecisionActorKind != tt.wantActorKind ||
					!got.TransitionAt.Equal(transitionAt) || got.DecisionCommentID != "") {
				t.Fatalf("implicit acceptance evidence = %#v", got)
			}
		})
	}
}

func TestManagerReturnsImplicitTransitionLookupError(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	wantErr := errors.New("target transition lookup failed")
	store := &faultAdmissionStore{Store: backend, targetTransitionErr: wantErr}
	manager := newAdmissionTestManager(
		t,
		admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
		store,
		func() time.Time { return now },
	)

	if _, err := manager.RunOnce(t.Context()); !errors.Is(err, wantErr) {
		t.Fatalf("RunOnce() error = %v, want %v", err, wantErr)
	}
}

func TestManagerAcceptanceTransitionsOrSupersedes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		current    string
		wantState  string
		wantStatus admissionmodel.ProposalStatus
		wantReason string
	}{
		{
			name:       "source issue is admitted",
			current:    "Backlog",
			wantState:  "Todo",
			wantStatus: admissionmodel.ProposalAccepted,
			wantReason: admissionResolutionExplicitAccept,
		},
		{
			name:       "issue that left source is superseded",
			current:    "In Progress",
			wantState:  "In Progress",
			wantStatus: admissionmodel.ProposalSuperseded,
			wantReason: admissionResolutionSourceStateChanged,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			currentTime := now
			issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
			tracker := memory.New(memory.Config{
				Issues:   []connector.Issue{issue},
				Stateful: true,
				Now:      func() time.Time { return currentTime },
			})
			backend := openManagerTestStore(t)
			proposal := admissionTestProposalForIssue("proposal-1", issue, now)
			if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
				t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
			}
			currentTime = now.Add(time.Minute)
			if err := tracker.CreateComment(t.Context(), issue.ID, admissionAcceptCommand(proposal.ID)); err != nil {
				t.Fatalf("CreateComment() error = %v", err)
			}
			if tt.current != issue.State {
				if err := tracker.UpdateIssueState(t.Context(), issue.ID, tt.current); err != nil {
					t.Fatalf("UpdateIssueState(%s) error = %v", tt.current, err)
				}
			}
			manager := newAdmissionTestManager(
				t,
				admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
				backend,
				func() time.Time { return currentTime },
			)

			if _, err := manager.RunOnce(t.Context()); err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(issues) != 1 || issues[0].State != tt.wantState {
				t.Fatalf("issue state = %#v, %v, want %s", issues, err, tt.wantState)
			}
			history, err := backend.AdmissionProposalHistory(t.Context(), proposal.ProjectID, issue.ID)
			if err != nil || len(history) != 1 {
				t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
			}
			if history[0].Status != tt.wantStatus || history[0].ResolutionReason != tt.wantReason {
				t.Fatalf("resolved proposal = %#v", history[0])
			}
			if tt.wantStatus != admissionmodel.ProposalAccepted {
				return
			}
			timeline, err := backend.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{ProjectID: proposal.ProjectID, IssueID: issue.ID})
			if err != nil || len(timeline.Events) != 1 {
				t.Fatalf("IssueWorkflowTimeline() = %#v, %v", timeline, err)
			}
			metadata, ok := provenance.Parse(timeline.Events[0].MetadataJSON)
			if !ok || metadata.Provenance.Actor == nil ||
				metadata.Provenance.Actor.Login != "memory" ||
				metadata.Admission == nil ||
				!metadata.Admission.Attributed ||
				metadata.Admission.ProposalID != proposal.ID {
				t.Fatalf("admission provenance = %#v", metadata)
			}
		})
	}
}

func TestManagerIgnoresUnauthorizedAdmissionDecision(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	decisionAt := now.Add(time.Minute)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	issue.Comments = []connector.IssueComment{{
		ID:               "decision-1",
		Backend:          connector.BackendGitHub.String(),
		Body:             admissionAcceptCommand("proposal-1"),
		AuthorLogin:      "outsider",
		AuthorKind:       "User",
		AuthorAuthorized: true,
		CreatedAt:        &decisionAt,
	}}
	tracker := &authorizingAdmissionIssueStore{
		Connector:  memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true}),
		authorized: false,
	}
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	proposal.CommentedAt = now
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	manager := newAdmissionTestManager(
		t,
		admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
		backend,
		func() time.Time { return decisionAt },
	)

	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(issues) != 1 || issues[0].State != "Backlog" {
		t.Fatalf("issue = %#v, %v", issues, err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 || history[0].Status != admissionmodel.ProposalOpen {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
}

func TestManagerAcceptanceRevalidatesImmediatelyBeforeMutation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	currentTime := now
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := &stateChangingAdmissionStore{
		Connector: memory.New(memory.Config{
			Issues:   []connector.Issue{issue},
			Stateful: true,
			Now:      func() time.Time { return currentTime },
		}),
		issueID: issue.ID,
		state:   "In Progress",
	}
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	currentTime = now.Add(time.Minute)
	if err := tracker.CreateComment(t.Context(), issue.ID, admissionAcceptCommand(proposal.ID)); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	manager := newAdmissionTestManager(
		t,
		admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
		backend,
		func() time.Time { return currentTime },
	)

	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	issues, err := tracker.Connector.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(issues) != 1 || issues[0].State != "In Progress" {
		t.Fatalf("issue = %#v, %v", issues, err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), proposal.ProjectID, issue.ID)
	if err != nil || len(history) != 1 ||
		history[0].Status != admissionmodel.ProposalSuperseded ||
		history[0].ResolutionReason != admissionResolutionSourceStateChanged {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
}

func TestManagerAutoAdmissionModes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		enabled    bool
		policies   map[string]bool
		labels     []string
		confidence float64
		wantState  string
		wantStatus admissionmodel.ProposalStatus
	}{
		{
			name:       "disabled requires explicit acceptance",
			confidence: 1,
			wantState:  "Backlog",
			wantStatus: admissionmodel.ProposalOpen,
		},
		{
			name:       "below threshold remains proposed",
			enabled:    true,
			confidence: 0.89,
			wantState:  "Backlog",
			wantStatus: admissionmodel.ProposalOpen,
		},
		{
			name:       "threshold match is admitted",
			enabled:    true,
			confidence: 0.9,
			wantState:  "Todo",
			wantStatus: admissionmodel.ProposalAccepted,
		},
		{
			name:       "defect class is auto-admitted",
			policies:   map[string]bool{"defect": true},
			labels:     []string{"defect"},
			confidence: 1,
			wantState:  "Todo",
			wantStatus: admissionmodel.ProposalAccepted,
		},
		{
			name:       "feature class is proposed and held regardless of confidence",
			enabled:    true,
			policies:   map[string]bool{"feature": false},
			labels:     []string{"feature"},
			confidence: 1,
			wantState:  "Backlog",
			wantStatus: admissionmodel.ProposalOpen,
		},
		{
			name:       "unknown label falls back to project default",
			policies:   map[string]bool{"defect": true},
			labels:     []string{"docs"},
			confidence: 1,
			wantState:  "Backlog",
			wantStatus: admissionmodel.ProposalOpen,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
			issue.Labels = append([]string(nil), tt.labels...)
			tracker := memory.New(memory.Config{
				Issues:   []connector.Issue{issue},
				Stateful: true,
				Now:      func() time.Time { return now },
			})
			backend := openManagerTestStore(t)
			agent := &scriptedAdmissionRunner{propose: proposeEveryCandidateAtConfidence(tt.confidence)}
			settings := admissionTestSettings(tracker, agent)
			settings.Config.AutoAdmit = tt.enabled
			settings.Config.AutoAdmitByLabel = tt.policies
			settings.Config.AutoAdmitMinConfidence = 0.9
			manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

			result, err := manager.RunOnce(t.Context())
			if err != nil || len(result.Proposals) != 1 {
				t.Fatalf("RunOnce() = %#v, %v", result, err)
			}
			issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(issues) != 1 || issues[0].State != tt.wantState {
				t.Fatalf("issue state = %#v, %v, want %s", issues, err, tt.wantState)
			}
			history, err := backend.AdmissionProposalHistory(t.Context(), "detent", issue.ID)
			if err != nil || len(history) != 1 || history[0].Status != tt.wantStatus {
				t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
			}
			if tt.wantStatus == admissionmodel.ProposalAccepted &&
				history[0].ResolutionReason != admissionResolutionAutoAdmit {
				t.Fatalf("auto-admitted proposal = %#v", history[0])
			}
		})
	}
}

func TestManagerAutoAdmissionRequiresEveryConfiguredDimension(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-590", "DD-590", 1, now)
	issue.Title = "Launch the complete marketing program"
	issue.Description = `Coordinate the website, campaign assets, launch emails, analytics, and partner outreach.

Child issues decompose the implementation work. Marketing and operations staff must approve copy, contact partners, and schedule the launch.`
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{issue},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	criteria := config.AdmissionCriteria{
		Section: "Admission criteria",
		Text: "- **Alignment** — serves a stated priority.\n" +
			"- **Readiness** — is a startable implementation unit.\n" +
			"- **Size** — fits in one pull request.\n" +
			"- **Safety Gates** — identifies required human actions.",
		Dimensions: []config.AdmissionDimension{
			{Name: "Alignment", Text: "- **Alignment** — serves a stated priority."},
			{Name: "Readiness", Text: "- **Readiness** — is a startable implementation unit."},
			{Name: "Size", Text: "- **Size** — fits in one pull request."},
			{Name: "Safety Gates", Text: "- **Safety Gates** — identifies required human actions."},
		},
	}
	agent := &scriptedAdmissionRunner{propose: func(request runner.RunRequest) []AgentProposal {
		return []AgentProposal{{
			IssueID: request.Admission.Candidates[0].ID,
			Findings: []admissionmodel.Finding{
				{Dimension: "Alignment", CriterionQuote: "serves a stated priority", Matched: true, Rationale: "The marketing launch supports a stated priority."},
				{Dimension: "Readiness", CriterionQuote: "is a startable implementation unit", Matched: false, Rationale: "The epic delegates implementation to child issues and has no PR-sized acceptance criteria."},
				{Dimension: "Size", CriterionQuote: "fits in one pull request", Matched: false, Rationale: "The website, campaign, email, analytics, and partner deliverables require separate changes."},
				{Dimension: "Safety Gates", CriterionQuote: "identifies required human actions", Matched: true, Rationale: "Copy approval, partner contact, and launch scheduling are explicit human actions."},
			},
			Confidence: float64Pointer(0.99),
		}}
	}}
	settings := admissionTestSettings(tracker, agent)
	settings.Criteria = criteria
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.9
	manager := newAdmissionTestManager(t, settings, openManagerTestStore(t), func() time.Time { return now })

	result, err := manager.RunOnce(t.Context())
	if err != nil || len(result.Proposals) != 1 {
		t.Fatalf("RunOnce() = %#v, %v", result, err)
	}
	if result.Proposals[0].Confidence != 0.99 || result.Proposals[0].Status != admissionmodel.ProposalOpen {
		t.Fatalf("proposal = %#v, want high-confidence open proposal", result.Proposals[0])
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(issues) != 1 || issues[0].State != "Backlog" {
		t.Fatalf("issue = %#v, %v, want Backlog", issues, err)
	}
	var comment string
	for _, event := range tracker.Events() {
		if event.Kind == memory.EventKindComment {
			comment = event.Body
		}
	}
	for _, want := range []string{
		"**Readiness** — **Failed.**",
		"**Size** — **Failed.**",
		"following required admission dimensions failed: **Readiness**, **Size**",
		"remains in **Backlog**",
	} {
		if !strings.Contains(comment, want) {
			t.Fatalf("proposal comment missing %q: %s", want, comment)
		}
	}
}

func TestManagerAutomaticAdmissionRechecksLabelPolicy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	issue.Labels = []string{"feature"}
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true, Now: func() time.Time { return now }})
	backend := openManagerTestStore(t)
	settings := admissionTestSettings(tracker, &scriptedAdmissionRunner{})
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitByLabel = map[string]bool{"feature": false}
	settings.Config.AutoAdmitMinConfidence = 0.9
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
	proposal := admissionTestProposalForIssue("proposal-1", issue, now)
	proposal.Confidence = 1
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}

	if err := manager.admitProposal(
		t.Context(),
		settings,
		connector.Issue{ID: issue.ID, State: "Backlog"},
		proposal,
		automaticAdmissionDecision(tracker, proposal, now),
	); err != nil {
		t.Fatalf("admitProposal() error = %v", err)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(issues) != 1 || issues[0].State != "Backlog" {
		t.Fatalf("issue state = %#v, %v", issues, err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), "detent", issue.ID)
	if err != nil || len(history) != 1 || history[0].Status != admissionmodel.ProposalSuperseded ||
		history[0].ResolutionReason != admissionResolutionAutoAdmitIneligible {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
}

func TestManagerRequiredEffortAdmissionModes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name              string
		requireEffort     bool
		body              string
		recommendedEffort string
		effortRationale   string
		wantState         string
		wantBody          string
		wantBodyUpdates   int
		wantInvalidOutput bool
		wantResolution    string
	}{
		{
			name:      "disabled preserves admission behavior",
			body:      "Actionable problem.",
			wantState: "Todo",
			wantBody:  "Actionable problem.",
		},
		{
			name:              "required recommendation unavailable fails evaluation",
			requireEffort:     true,
			body:              "Actionable problem.",
			wantState:         "Backlog",
			wantBody:          "Actionable problem.",
			wantInvalidOutput: true,
		},
		{
			name:              "required recommendation writes absent block",
			requireEffort:     true,
			body:              "Actionable problem.",
			recommendedEffort: "high",
			effortRationale:   "The change crosses admission and dispatch.",
			wantState:         "Todo",
			wantBody:          "Actionable problem.\n\n```detent-agent\nschema: 1\neffort: high\n```\n",
			wantBodyUpdates:   1,
		},
		{
			name:              "existing block remains authoritative",
			requireEffort:     true,
			body:              "Actionable problem.\n\n```detent-agent\nschema: 1\neffort: xhigh\n```\n",
			recommendedEffort: "high",
			effortRationale:   "The agent recommends the standard effort.",
			wantState:         "Todo",
			wantBody:          "Actionable problem.\n\n```detent-agent\nschema: 1\neffort: xhigh\n```\n",
		},
		{
			name:              "malformed existing block prevents admission",
			requireEffort:     true,
			body:              "Actionable problem.\n\n```detent-agent\nschema: 2\neffort: xhigh\n```\n",
			recommendedEffort: "high",
			effortRationale:   "The agent recommends the standard effort.",
			wantState:         "Backlog",
			wantBody:          "Actionable problem.\n\n```detent-agent\nschema: 2\neffort: xhigh\n```\n",
			wantResolution:    admissionResolutionEffortUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
			issue.Description = tt.body
			tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
			agent := &scriptedAdmissionRunner{propose: func(request runner.RunRequest) []AgentProposal {
				proposals := proposeEveryCandidateAtConfidence(1)(request)
				proposals[0].RecommendedEffort = tt.recommendedEffort
				proposals[0].EffortRationale = tt.effortRationale
				return proposals
			}}
			settings := admissionTestSettings(tracker, agent)
			settings.Config.AutoAdmit = true
			settings.Config.AutoAdmitMinConfidence = 0.9
			settings.Config.RequireEffort = tt.requireEffort
			settings.Config.EffortSection = "Issue effort selection"
			settings.EffortRubric = admissionTestEffortRubric()
			manager := newAdmissionTestManager(t, settings, openManagerTestStore(t), func() time.Time { return now })

			result, err := manager.RunOnce(t.Context())
			if tt.wantInvalidOutput && !errors.Is(err, ErrInvalidOutput) {
				t.Fatalf("RunOnce() error = %v, want ErrInvalidOutput", err)
			}
			if !tt.wantInvalidOutput && err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(issues) != 1 {
				t.Fatalf("FetchIssueStatesByIDs() = %#v, %v", issues, err)
			}
			if issues[0].State != tt.wantState || issues[0].Description != tt.wantBody {
				t.Fatalf("issue = %#v, want state %q body %q", issues[0], tt.wantState, tt.wantBody)
			}
			bodyUpdates := 0
			for _, event := range tracker.Events() {
				if event.Kind == memory.EventKindBodyUpdate {
					bodyUpdates++
				}
			}
			if bodyUpdates != tt.wantBodyUpdates {
				t.Fatalf("result = %#v, body updates = %d", result, bodyUpdates)
			}
			if tt.wantResolution != "" {
				history, err := manager.store.AdmissionProposalHistory(t.Context(), "detent", issue.ID)
				if err != nil || len(history) != 1 ||
					history[0].Status != admissionmodel.ProposalSuperseded ||
					history[0].ResolutionReason != tt.wantResolution {
					t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
				}
			}
		})
	}
}

func TestManagerWritesEffortBeforeDispatchResolvesSession(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	settings := admissionTestSettings(
		tracker,
		&scriptedAdmissionRunner{propose: proposeEveryCandidateWithEffort("high")},
	)
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.9
	settings.Config.RequireEffort = true
	settings.Config.EffortSection = "Issue effort selection"
	settings.EffortRubric = admissionTestEffortRubric()
	manager := newAdmissionTestManager(t, settings, openManagerTestStore(t), func() time.Time { return now })

	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	events := tracker.Events()
	bodyIndex, stateIndex := -1, -1
	for index, event := range events {
		switch event.Kind {
		case memory.EventKindBodyUpdate:
			bodyIndex = index
		case memory.EventKindStateUpdate:
			stateIndex = index
		}
	}
	if bodyIndex < 0 || stateIndex < 0 || bodyIndex >= stateIndex {
		t.Fatalf("events = %#v, want body update before state transition", events)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(issues) != 1 {
		t.Fatalf("FetchIssueStatesByIDs() = %#v, %v", issues, err)
	}
	agentBackend := &effortRecordingAgentBackend{}
	dispatchRunner, err := runner.NewRunner(runner.Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}, Prompt: "Implement the admitted issue."},
		Workspace:    &staticAdmissionDispatchWorkspace{path: t.TempDir()},
		AgentBackend: agentBackend,
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("runner.NewRunner() error = %v", err)
	}
	if _, err := dispatchRunner.Run(t.Context(), runner.RunRequest{
		Issue:     issues[0],
		Mode:      runner.RunModeImplement,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("dispatch Run() error = %v", err)
	}
	if agentBackend.request.ReasoningEffort != "high" {
		t.Fatalf("dispatched effort = %q, want written high", agentBackend.request.ReasoningEffort)
	}
}

func TestManagerRequiredEffortSupersedesOpenProposalWithoutRecommendation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-open", issue, now.Add(-time.Minute))
	proposal.Confidence = 1
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	settings := admissionTestSettings(tracker, staticAdmissionRunner{output: `{"evaluations":[{"issue_id":"issue-1","disposition":"declined","findings":[{"dimension":"Alignment","criterion_quote":"serves a stated current priority","matched":false,"rationale":"The issue does not serve a stated current priority."},{"dimension":"Readiness","criterion_quote":"has an actionable problem statement","matched":false,"rationale":"The issue lacks an actionable problem statement."}],"confidence":0.2}]}`})
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.9
	settings.Config.RequireEffort = true
	settings.Config.EffortSection = "Issue effort selection"
	settings.EffortRubric = admissionTestEffortRubric()
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(issues) != 1 || issues[0].State != "Backlog" {
		t.Fatalf("issue = %#v, %v", issues, err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), "detent", issue.ID)
	if err != nil || len(history) != 1 ||
		history[0].Status != admissionmodel.ProposalSuperseded ||
		history[0].ResolutionReason != admissionResolutionEffortUnavailable {
		t.Fatalf("history = %#v, %v", history, err)
	}
}

func TestManagerRecoversAutoAdmissionAfterResolutionFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{issue},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	backend := openManagerTestStore(t)
	failingStore := &failingAdmissionResolutionStore{Store: backend, fail: true}
	settings := admissionTestSettings(
		tracker,
		&scriptedAdmissionRunner{propose: proposeEveryCandidateAtConfidence(1)},
	)
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.9
	manager := newAdmissionTestManager(t, settings, failingStore, func() time.Time { return now })

	if _, err := manager.RunOnce(t.Context()); !errors.Is(err, errAdmissionResolutionFailure) {
		t.Fatalf("first RunOnce() error = %v, want %v", err, errAdmissionResolutionFailure)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(issues) != 1 || issues[0].State != "Todo" {
		t.Fatalf("issue after failed resolution = %#v, %v", issues, err)
	}
	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), "detent", issue.ID)
	if err != nil || len(history) != 1 ||
		history[0].Status != admissionmodel.ProposalAccepted ||
		history[0].ResolutionReason != admissionResolutionAutoAdmit {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
	stateUpdates := 0
	for _, event := range tracker.Events() {
		if event.Kind == memory.EventKindStateUpdate {
			stateUpdates++
		}
	}
	if stateUpdates != 1 {
		t.Fatalf("state update events = %d, want 1", stateUpdates)
	}
}

func TestManagerAutoAdmissionRespectsEligibilityAndCaps(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	first := admissionIssueFixture("issue-1", "DD-1", 1, now)
	first.AuthorID = "octocat"
	second := admissionIssueFixture("issue-2", "DD-2", 2, now)
	second.AuthorID = "octocat"
	excluded := admissionIssueFixture("issue-3", "DD-3", 3, now)
	excluded.AuthorID = "octocat"
	excluded.Labels = []string{"do-not-admit"}
	otherAuthor := admissionIssueFixture("issue-4", "DD-4", 4, now)
	otherAuthor.AuthorID = "hubot"
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{first, second, excluded, otherAuthor},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.8
	settings.Config.MaxProposalsPerRun = 1
	settings.Config.MaxOpenProposals = 1
	settings.Config.ExcludeLabels = []string{"do-not-admit"}
	settings.Config.Authors.Allow = []string{"octocat"}
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Skipped["excluded_label"] != 1 || result.Skipped["author"] != 1 ||
		result.Truncated["candidates"] != 1 {
		t.Fatalf("result = %#v", result)
	}
	issues, err := tracker.FetchIssueStatesByIDs(
		t.Context(),
		[]string{first.ID, second.ID, excluded.ID, otherAuthor.ID},
	)
	if err != nil || len(issues) != 4 {
		t.Fatalf("FetchIssueStatesByIDs() = %#v, %v", issues, err)
	}
	states := map[string]string{}
	for _, issue := range issues {
		states[issue.ID] = issue.State
	}
	if states[first.ID] != "Todo" ||
		states[second.ID] != "Backlog" ||
		states[excluded.ID] != "Backlog" ||
		states[otherAuthor.ID] != "Backlog" {
		t.Fatalf("states = %#v", states)
	}
}

func TestManagerAutoAdmissionRespectsOpenProposalCap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	first := admissionIssueFixture("issue-1", "DD-1", 1, now)
	second := admissionIssueFixture("issue-2", "DD-2", 2, now)
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{first, second},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-open", first, now)
	proposal.Confidence = 0.5
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.9
	settings.Config.MaxOpenProposals = 1
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if agent.calls != 0 || result.Skipped["open_proposal_cap"] != 1 {
		t.Fatalf("result = %#v, runner calls = %d", result, agent.calls)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{first.ID, second.ID})
	if err != nil || len(issues) != 2 || issues[0].State != "Backlog" || issues[1].State != "Backlog" {
		t.Fatalf("issues = %#v, %v", issues, err)
	}
}

func TestManagerImplicitAcceptanceReleasesOpenProposalCapacity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	first := admissionIssueFixture("issue-1", "DD-1", 1, now)
	proposal := admissionTestProposalForIssue("proposal-open", first, now)
	transitionAt := now.Add(time.Minute)
	first.State = proposal.TargetState
	first.StageUpdatedAt = &transitionAt
	second := admissionIssueFixture("issue-2", "DD-2", 2, now)
	tracker := &transitionAdmissionIssueStore{
		Connector: memory.New(memory.Config{Issues: []connector.Issue{first, second}, Stateful: true}),
		transition: connector.IssueStateTransition{
			EnteredAt: transitionAt,
			Actor:     connector.IssueActor{Login: "ada", Kind: "User"},
		},
		found: true,
	}
	backend := openManagerTestStore(t)
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.MaxOpenProposals = 1
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return transitionAt })

	result, err := manager.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if agent.calls != 1 || len(result.Proposals) != 1 || result.Proposals[0].IssueID != second.ID {
		t.Fatalf("result = %#v, runner calls = %d", result, agent.calls)
	}
	open, err := backend.OpenAdmissionProposals(t.Context(), proposal.ProjectID, 0)
	if err != nil || len(open) != 1 || open[0].IssueID != second.ID {
		t.Fatalf("OpenAdmissionProposals() = %#v, %v", open, err)
	}
}

func TestManagerAutoAdmissionSupersedesIneligibleOpenProposal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	original := admissionIssueFixture("issue-1", "DD-1", 1, now)
	current := original
	current.Labels = []string{"do-not-admit"}
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{current},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-open", original, now)
	proposal.Confidence = 1
	if created, err := backend.CreateAdmissionProposal(t.Context(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	settings := admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate})
	settings.Config.AutoAdmit = true
	settings.Config.AutoAdmitMinConfidence = 0.9
	settings.Config.ExcludeLabels = []string{"do-not-admit"}
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	history, err := backend.AdmissionProposalHistory(t.Context(), proposal.ProjectID, proposal.IssueID)
	if err != nil || len(history) != 1 ||
		history[0].Status != admissionmodel.ProposalSuperseded ||
		history[0].ResolutionReason != admissionResolutionAutoAdmitIneligible {
		t.Fatalf("AdmissionProposalHistory() = %#v, %v", history, err)
	}
	issues, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{proposal.IssueID})
	if err != nil || len(issues) != 1 || issues[0].State != "Backlog" {
		t.Fatalf("issue = %#v, %v", issues, err)
	}
}

func TestManagerFiltersProposalHistoryBeforeCandidateCap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issues := []connector.Issue{
		admissionIssueFixture("issue-1", "DD-1", 1, now),
		admissionIssueFixture("issue-2", "DD-2", 2, now),
		admissionIssueFixture("issue-3", "DD-3", 3, now),
	}
	tracker := memory.New(memory.Config{Issues: issues, Stateful: true})
	backend := openManagerTestStore(t)
	proposal := admissionTestProposalForIssue("proposal-accepted", issues[0], now.Add(-time.Hour))
	if created, err := backend.CreateAdmissionProposal(context.Background(), proposal); err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %t, %v", created, err)
	}
	if err := backend.TransitionAdmissionProposal(
		context.Background(),
		proposal.ID,
		admissionmodel.ProposalOpen,
		admissionmodel.ProposalAccepted,
		now,
	); err != nil {
		t.Fatalf("TransitionAdmissionProposal() error = %v", err)
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.MaxCandidatesPerRun = 1
	settings.Config.MaxProposalsPerRun = 1
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if got, want := agent.candidateIDs[0], []string{"issue-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate ids = %#v, want %#v", got, want)
	}
	if result.Skipped["accepted_demotion"] != 1 || result.Truncated["candidates"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestManagerAcceptedDemotionIsSticky(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	current := now
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{admissionIssueFixture("issue-1", "DD-1", 1, now)},
		Stateful: true,
		Now:      func() time.Time { return current },
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return current })
	if result, err := manager.RunOnce(context.Background()); err != nil || len(result.Proposals) != 1 {
		t.Fatalf("initial RunOnce() = %#v, %v", result, err)
	}
	open, err := backend.OpenAdmissionProposals(context.Background(), "detent", 0)
	if err != nil || len(open) != 1 {
		t.Fatalf("OpenAdmissionProposals() = %#v, %v", open, err)
	}
	current = now.Add(time.Minute)
	if err := tracker.CreateComment(context.Background(), "issue-1", admissionAcceptCommand(open[0].ID)); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if _, err := manager.RunOnce(context.Background()); err != nil {
		t.Fatalf("acceptance reconciliation error = %v", err)
	}
	admitted, err := tracker.FetchIssueStatesByIDs(context.Background(), []string{"issue-1"})
	if err != nil || len(admitted) != 1 || admitted[0].State != "Todo" {
		t.Fatalf("admitted issue = %#v, %v, want Todo", admitted, err)
	}
	if err := tracker.UpdateIssueState(context.Background(), "issue-1", "Backlog"); err != nil {
		t.Fatalf("UpdateIssueState(Backlog) error = %v", err)
	}
	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("demotion RunOnce() error = %v", err)
	}
	if agent.calls != 1 || result.Skipped["accepted_demotion"] != 1 {
		t.Fatalf("demotion result = %#v, runner calls = %d", result, agent.calls)
	}
	history, err := backend.AdmissionProposalHistory(context.Background(), "detent", "issue-1")
	if err != nil || len(history) != 1 || history[0].Status != admissionmodel.ProposalAccepted {
		t.Fatalf("history = %#v, %v", history, err)
	}
}

func TestManagerDefersForCapacityAndBudget(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{admissionIssueFixture("issue-1", "DD-1", 1, now)}, Stateful: true})
	tests := []struct {
		name     string
		settings func(*scriptedAdmissionRunner) Settings
		want     string
	}{
		{
			name: "capacity",
			settings: func(agent *scriptedAdmissionRunner) Settings {
				settings := admissionTestSettings(tracker, agent)
				localScheduler := scheduler.NewCountingSemaphore(scheduler.Config{Capacity: 1})
				if _, err := localScheduler.RequestSlot(context.Background(), scheduler.SlotRequest{State: "Todo"}); err != nil {
					t.Fatalf("hold scheduler slot: %v", err)
				}
				settings.Scheduler = localScheduler
				return settings
			},
			want: "fleet_capacity",
		},
		{
			name: "budget",
			settings: func(agent *scriptedAdmissionRunner) Settings {
				settings := admissionTestSettings(tracker, &budgetAdmissionRunner{
					scriptedAdmissionRunner: agent,
					status: runner.DailyBudgetStatus{
						Active:          true,
						CurrentSpendUSD: 100,
						MaxUSD:          100,
					},
				})
				return settings
			},
			want: "budget",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := openManagerTestStore(t)
			agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
			manager := newAdmissionTestManager(t, tt.settings(agent), backend, func() time.Time { return now })
			result, err := manager.RunOnce(context.Background())
			if err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if result.DeferredReason != tt.want || agent.calls != 0 {
				t.Fatalf("result = %#v, runner calls = %d", result, agent.calls)
			}
		})
	}
}

func TestAcquireCapacityReleaseClearsDerivedAdmissionReservation(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		pool string
	}{
		{name: "default pool", pool: scheduler.DefaultPoolName},
		{name: "non-default pool", pool: "video"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 7, 31, 13, 2, 34, 0, time.UTC)
			higher := scheduler.ProjectCandidate{ID: "detent", Pool: tt.pool, Priority: 0}
			lower := scheduler.ProjectCandidate{ID: "gopher-ai", Pool: tt.pool, Priority: 3}
			pools := []scheduler.PoolConfig{{
				Name:      scheduler.DefaultPoolName,
				Scheduler: scheduler.Config{Kind: "strict", Capacity: 1},
			}}
			if tt.pool != scheduler.DefaultPoolName {
				pools = append(pools, scheduler.PoolConfig{
					Name:      tt.pool,
					Scheduler: scheduler.Config{Kind: "strict", Capacity: 1},
				})
			}
			gate, err := scheduler.NewPoolRegistry(pools, []scheduler.ProjectCandidate{higher, lower})
			if err != nil {
				t.Fatalf("NewPoolRegistry() error = %v", err)
			}
			gate.BeginProjectCycle(higher)
			gate.EndProjectCycle(higher.ID)
			settings := Settings{
				GlobalDispatchGate: gate,
				ProjectCandidate:   higher,
			}

			for run := 1; run <= 2; run++ {
				release, acquired, reason, err := acquireCapacity(t.Context(), settings, now.Add(time.Duration(run)*time.Minute))
				if err != nil {
					t.Fatalf("admission run %d acquireCapacity() error = %v", run, err)
				}
				if !acquired || reason != "" {
					t.Fatalf("admission run %d acquireCapacity() = %t, %q, want true, empty reason", run, acquired, reason)
				}
				if err := release(); err != nil {
					t.Fatalf("admission run %d release() error = %v", run, err)
				}

				slot, granted, decision, err := gate.TryAcquireWithDecision(
					t.Context(),
					lower,
					scheduler.SlotRequest{State: "Todo"},
					now.Add(time.Duration(run)*time.Minute+time.Second),
				)
				if err != nil {
					t.Fatalf("lower run %d TryAcquireWithDecision() error = %v", run, err)
				}
				if !granted {
					t.Fatalf("lower run %d TryAcquireWithDecision() decision = %#v, want granted", run, decision)
				}
				if err := gate.Release(slot); err != nil {
					t.Fatalf("lower run %d Release() error = %v", run, err)
				}
			}
		})
	}
}

func TestAcquireCapacityReleaseErrorPreservesDerivedAdmissionDemand(t *testing.T) {
	t.Parallel()

	releaseErr := errors.New("release slot")
	now := time.Date(2026, 7, 31, 13, 2, 34, 0, time.UTC)
	higher := scheduler.ProjectCandidate{ID: "detent", Priority: 0}
	lower := scheduler.ProjectCandidate{ID: "gopher-ai", Priority: 3}
	global := &releaseErrorGlobalScheduler{
		GlobalScheduler: scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}),
		err:             releaseErr,
	}
	gate := scheduler.NewGlobalDispatchGate(global, higher, lower)
	gate.BeginProjectCycle(higher)
	gate.EndProjectCycle(higher.ID)

	release, acquired, reason, err := acquireCapacity(t.Context(), Settings{
		GlobalDispatchGate: gate,
		ProjectCandidate:   higher,
	}, now)
	if err != nil {
		t.Fatalf("acquireCapacity() error = %v", err)
	}
	if !acquired || reason != "" {
		t.Fatalf("acquireCapacity() = %t, %q, want true, empty reason", acquired, reason)
	}
	if err := release(); !errors.Is(err, releaseErr) {
		t.Fatalf("release() error = %v, want %v", err, releaseErr)
	}

	_, granted, decision, err := gate.TryAcquireWithDecision(
		t.Context(),
		lower,
		scheduler.SlotRequest{State: "Todo"},
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("lower TryAcquireWithDecision() error = %v", err)
	}
	if granted || decision.Reason != scheduler.DispatchGateReasonReservedForHigherPriorityProject {
		t.Fatalf("lower TryAcquireWithDecision() decision = %#v, want priority reservation", decision)
	}
}

func TestAcquireCapacityFailedAcquireClearsDerivedAdmissionReservation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 13, 2, 34, 0, time.UTC)
	higher := scheduler.ProjectCandidate{ID: "detent", Pool: "video", Priority: 0}
	lower := scheduler.ProjectCandidate{ID: "gopher-ai", Pool: "video", Priority: 3}
	gate, err := scheduler.NewPoolRegistry(
		[]scheduler.PoolConfig{
			{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "strict", Capacity: 1}},
			{Name: "video", Scheduler: scheduler.Config{Kind: "strict", Capacity: 1}},
		},
		[]scheduler.ProjectCandidate{higher, lower},
	)
	if err != nil {
		t.Fatalf("NewPoolRegistry() error = %v", err)
	}
	gate.BeginProjectCycle(higher)
	gate.EndProjectCycle(higher.ID)
	resumeDispatch := gate.PauseDispatch()
	release, acquired, reason, err := acquireCapacity(t.Context(), Settings{
		GlobalDispatchGate: gate,
		ProjectCandidate:   higher,
	}, now)
	resumeDispatch()
	if err != nil {
		t.Fatalf("acquireCapacity() error = %v", err)
	}
	if acquired || reason != "fleet_capacity" {
		t.Fatalf("acquireCapacity() = %t, %q, want false, fleet_capacity", acquired, reason)
	}
	if err := release(); err != nil {
		t.Fatalf("release() error = %v", err)
	}

	slot, granted, decision, err := gate.TryAcquireWithDecision(
		t.Context(),
		lower,
		scheduler.SlotRequest{State: "Todo"},
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("lower TryAcquireWithDecision() error = %v", err)
	}
	if !granted {
		t.Fatalf("lower TryAcquireWithDecision() decision = %#v, want granted", decision)
	}
	if err := gate.Release(slot); err != nil {
		t.Fatalf("lower Release() error = %v", err)
	}
}

func TestAcquireCapacitySafetyBoundaries(t *testing.T) {
	t.Parallel()

	errRequest := errors.New("request failed")
	errLocalRelease := errors.New("local release failed")
	errGate := errors.New("gate failed")
	errGateRelease := errors.New("gate release failed")

	tests := []struct {
		name             string
		settings         Settings
		wantAcquired     bool
		wantReason       string
		wantAcquireErrs  []error
		wantReleaseErrs  []error
		wantMarkedIdle   int
		wantLocalRelease int
		wantGateRelease  int
	}{
		{
			name:       "local capacity full",
			settings:   Settings{Scheduler: &capacityScheduler{requestErr: scheduler.ErrNoSlots}},
			wantReason: "fleet_capacity",
		},
		{
			name:            "local request error",
			settings:        Settings{Scheduler: &capacityScheduler{requestErr: errRequest}},
			wantAcquireErrs: []error{errRequest},
		},
		{
			name:             "local capacity only",
			settings:         Settings{Scheduler: &capacityScheduler{}},
			wantAcquired:     true,
			wantLocalRelease: 1,
		},
		{
			name: "missing project candidate releases local capacity",
			settings: Settings{
				Scheduler:          &capacityScheduler{releaseErr: errLocalRelease},
				GlobalDispatchGate: &capacityGate{},
			},
			wantAcquireErrs:  []error{errLocalRelease},
			wantLocalRelease: 1,
		},
		{
			name: "gate request error releases local capacity",
			settings: Settings{
				Scheduler:          &capacityScheduler{releaseErr: errLocalRelease},
				GlobalDispatchGate: &capacityGate{tryErr: errGate},
				ProjectCandidate:   scheduler.ProjectCandidate{ID: "detent"},
			},
			wantAcquireErrs:  []error{errGate, errLocalRelease},
			wantLocalRelease: 1,
		},
		{
			name: "gate refusal marks candidate idle",
			settings: Settings{
				Scheduler:          &capacityScheduler{},
				GlobalDispatchGate: &capacityGate{},
				ProjectCandidate:   scheduler.ProjectCandidate{ID: "detent"},
			},
			wantReason:       "fleet_capacity",
			wantMarkedIdle:   1,
			wantLocalRelease: 1,
		},
		{
			name: "gate release error preserves demand",
			settings: Settings{
				Scheduler:          &capacityScheduler{releaseErr: errLocalRelease},
				GlobalDispatchGate: &capacityGate{acquired: true, releaseErr: errGateRelease},
				ProjectCandidate:   scheduler.ProjectCandidate{ID: "detent"},
			},
			wantAcquired:     true,
			wantReleaseErrs:  []error{errGateRelease, errLocalRelease},
			wantLocalRelease: 1,
			wantGateRelease:  1,
		},
		{
			name: "successful cleanup marks candidate idle",
			settings: Settings{
				Scheduler:          &capacityScheduler{},
				GlobalDispatchGate: &capacityGate{acquired: true},
				ProjectCandidate:   scheduler.ProjectCandidate{ID: " detent "},
			},
			wantAcquired:     true,
			wantMarkedIdle:   1,
			wantLocalRelease: 1,
			wantGateRelease:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			release, acquired, reason, acquireErr := acquireCapacity(t.Context(), tt.settings, time.Now())
			if acquired != tt.wantAcquired || reason != tt.wantReason {
				t.Fatalf("acquireCapacity() = %t, %q, %v", acquired, reason, acquireErr)
			}
			if len(tt.wantAcquireErrs) == 0 && acquireErr != nil {
				t.Fatalf("acquireCapacity() error = %v", acquireErr)
			}
			for _, wantErr := range tt.wantAcquireErrs {
				if !errors.Is(acquireErr, wantErr) {
					t.Fatalf("acquireCapacity() error = %v, want %v", acquireErr, wantErr)
				}
			}
			if acquired {
				releaseErr := release()
				if len(tt.wantReleaseErrs) == 0 && releaseErr != nil {
					t.Fatalf("release() error = %v", releaseErr)
				}
				for _, wantErr := range tt.wantReleaseErrs {
					if !errors.Is(releaseErr, wantErr) {
						t.Fatalf("release() error = %v, want %v", releaseErr, wantErr)
					}
				}
			}
			if local, ok := tt.settings.Scheduler.(*capacityScheduler); ok && local.releases != tt.wantLocalRelease {
				t.Fatalf("local releases = %d, want %d", local.releases, tt.wantLocalRelease)
			}
			if gate, ok := tt.settings.GlobalDispatchGate.(*capacityGate); ok {
				if len(gate.markedIdle) != tt.wantMarkedIdle || gate.releases != tt.wantGateRelease {
					t.Fatalf("gate marked idle = %d releases = %d", len(gate.markedIdle), gate.releases)
				}
				for _, candidate := range gate.markedIdle {
					if candidate.ID != "detent/admission" {
						t.Fatalf("idle candidate = %#v", candidate)
					}
				}
			}
		})
	}
}

func TestManagerCoverageFloorBoundaries(t *testing.T) {
	t.Parallel()

	var nilManager *Manager
	if err := nilManager.Update(Settings{}); !errors.Is(err, ErrMissingStore) {
		t.Fatalf("nil Update() error = %v", err)
	}
	if nilManager.Enabled() {
		t.Fatal("nil Enabled() = true")
	}

	now := time.Date(2026, 7, 31, 15, 0, 0, 0, time.UTC)
	tracker := memory.New(memory.Config{Stateful: true})
	backend := openManagerTestStore(t)
	runBackend := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	valid := admissionTestSettings(tracker, runBackend)

	if _, err := New(valid, nil, nil, nil); !errors.Is(err, ErrMissingStore) {
		t.Fatalf("New() missing store error = %v", err)
	}

	tests := []struct {
		name     string
		settings func() Settings
		store    Store
		want     string
	}{
		{
			name: "missing runner",
			settings: func() Settings {
				settings := valid
				settings.Runner = nil
				return settings
			},
			store: backend,
			want:  ErrMissingRunner.Error(),
		},
		{
			name: "missing issue store",
			settings: func() Settings {
				settings := valid
				settings.Issues = nil
				return settings
			},
			store: backend,
			want:  ErrMissingIssueStore.Error(),
		},
		{
			name: "missing criteria",
			settings: func() Settings {
				settings := valid
				settings.Criteria = config.AdmissionCriteria{}
				return settings
			},
			store: backend,
			want:  "criteria are unresolved",
		},
		{
			name: "missing effort rubric",
			settings: func() Settings {
				settings := valid
				settings.Config.RequireEffort = true
				return settings
			},
			store: backend,
			want:  "effort rubric is unresolved",
		},
		{
			name: "missing issue body updater",
			settings: func() Settings {
				settings := valid
				settings.Config.RequireEffort = true
				settings.EffortRubric = admissionTestEffortRubric()
				settings.Issues = &selectorAdmissionIssueStore{IssueStore: tracker}
				return settings
			},
			store: backend,
			want:  "issue body updater is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			manager := &Manager{store: tt.store, now: func() time.Time { return now }, updates: make(chan struct{}, 1)}
			if err := manager.Update(tt.settings()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Update() error = %v, want %q", err, tt.want)
			}
		})
	}

	disabled, err := New(Settings{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("New(disabled) error = %v", err)
	}
	if _, err := disabled.RunOnce(context.Background()); err != nil {
		t.Fatalf("disabled RunOnce() error = %v", err)
	}
	disabled.runAndLog(t.Context(), now)

	manager := newAdmissionTestManager(t, valid, backend, func() time.Time { return now })
	manager.settings.Config.Schedule = "invalid"
	if _, err := manager.run(t.Context(), now, true); err == nil {
		t.Fatal("run(invalid schedule) error = nil")
	}
	manager.runAndLog(t.Context(), now)
	manager.settings.Config.Schedule = valid.Config.Schedule
	if result, err := manager.run(t.Context(), time.Time{}, true); err != nil || result.Candidates != 0 {
		t.Fatalf("run(stale schedule) = %#v, %v", result, err)
	}

	faults := &faultAdmissionStore{Store: backend, latestErr: errors.New("latest failed")}
	errorManager := newAdmissionTestManager(t, valid, faults, func() time.Time { return now })
	if _, _, err := errorManager.nextScheduled(t.Context()); !errors.Is(err, faults.latestErr) {
		t.Fatalf("nextScheduled() error = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := errorManager.Run(canceled); err != nil {
		t.Fatalf("Run(latest error) error = %v", err)
	}

	faults.latestErr = nil
	faults.latestOK = true
	faults.latest = admissionmodel.RunRecord{CompletedAt: now.Add(time.Hour)}
	next, scheduled, err := errorManager.nextScheduled(t.Context())
	if err != nil || !scheduled || !next.After(faults.latest.CompletedAt) {
		t.Fatalf("nextScheduled() = %s, %t, %v", next, scheduled, err)
	}
	errorManager.settings.Config.Schedule = "invalid"
	if _, _, err := errorManager.nextScheduled(t.Context()); err == nil {
		t.Fatal("nextScheduled(invalid schedule) error = nil")
	}

	timerStore := &faultAdmissionStore{
		Store:    backend,
		latestOK: true,
		latest:   admissionmodel.RunRecord{CompletedAt: now},
	}
	timerManager := newAdmissionTestManager(t, valid, timerStore, func() time.Time { return now })
	select {
	case <-timerManager.updates:
	default:
		t.Fatal("new manager did not signal an update")
	}
	if err := timerManager.Run(canceled); err != nil {
		t.Fatalf("Run(canceled scheduled) error = %v", err)
	}
	stopTimer(nil)
	stopTimer(time.NewTimer(time.Hour))
}

func TestManagerRunOnceBoundaryErrors(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 16, 0, 0, 0, time.UTC)
	baseTracker := memory.New(memory.Config{Stateful: true})
	baseBackend := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	errBoundary := errors.New("boundary failed")

	tests := []struct {
		name     string
		prepare  func(*testing.T, Settings, Store) (Settings, Store)
		wantText string
	}{
		{
			name: "expire proposals",
			prepare: func(_ *testing.T, settings Settings, backend Store) (Settings, Store) {
				return settings, &faultAdmissionStore{Store: backend, expireErr: errBoundary}
			},
		},
		{
			name: "refresh outcomes",
			prepare: func(_ *testing.T, settings Settings, backend Store) (Settings, Store) {
				return settings, &faultAdmissionStore{Store: backend, refreshErr: errBoundary}
			},
		},
		{
			name: "budget lookup",
			prepare: func(_ *testing.T, settings Settings, backend Store) (Settings, Store) {
				settings.Runner = &boundaryBudgetRunner{Backend: settings.Runner, err: errBoundary}
				return settings, backend
			},
			wantText: "read backlog admission budget status",
		},
		{
			name: "capacity request",
			prepare: func(_ *testing.T, settings Settings, backend Store) (Settings, Store) {
				settings.Scheduler = &capacityScheduler{requestErr: errBoundary}
				return settings, backend
			},
		},
		{
			name: "candidate read",
			prepare: func(_ *testing.T, settings Settings, backend Store) (Settings, Store) {
				settings.Issues = &failingCandidateIssueStore{IssueStore: settings.Issues, err: errBoundary}
				return settings, backend
			},
			wantText: "fetch backlog admission candidates",
		},
		{
			name: "record run",
			prepare: func(_ *testing.T, settings Settings, backend Store) (Settings, Store) {
				return settings, &faultAdmissionStore{Store: backend, recordErr: errBoundary}
			},
			wantText: "record backlog admission run",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := openManagerTestStore(t)
			settings := admissionTestSettings(baseTracker, baseBackend)
			settings, wrapped := tt.prepare(t, settings, backend)
			manager := newAdmissionTestManager(t, settings, wrapped, func() time.Time { return now })
			_, err := manager.RunOnce(t.Context())
			if !errors.Is(err, errBoundary) || tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("RunOnce() error = %v, want %q", err, tt.wantText)
			}
		})
	}
}

func TestManagerOrderingAndParsingBoundaries(t *testing.T) {
	t.Parallel()

	priorityOne, priorityTwo := 1, 2
	earlier := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)
	settings := Settings{DispatchStates: []string{"Todo", "Backlog"}, DispatchLabels: []string{"urgent", "normal"}, PrioritizeBlockers: true}
	tests := []struct {
		name  string
		left  connector.Issue
		right connector.Issue
	}{
		{name: "state", left: connector.Issue{Identifier: "left", State: "Todo"}, right: connector.Issue{Identifier: "right", State: "Backlog"}},
		{name: "priority", left: connector.Issue{Identifier: "left", State: "Todo", Priority: &priorityOne}, right: connector.Issue{Identifier: "right", State: "Todo", Priority: &priorityTwo}},
		{name: "labeled", left: connector.Issue{Identifier: "left", State: "Todo", Labels: []string{"urgent"}}, right: connector.Issue{Identifier: "right", State: "Todo"}},
		{name: "label rank", left: connector.Issue{Identifier: "left", State: "Todo", Labels: []string{"urgent"}}, right: connector.Issue{Identifier: "right", State: "Todo", Labels: []string{"normal"}}},
		{name: "blockers", left: connector.Issue{Identifier: "left", State: "Todo", UnblockerCount: 2}, right: connector.Issue{Identifier: "right", State: "Todo", UnblockerCount: 1}},
		{name: "created", left: connector.Issue{Identifier: "left", State: "Todo", CreatedAt: &earlier}, right: connector.Issue{Identifier: "right", State: "Todo", CreatedAt: &later}},
		{name: "left created", left: connector.Issue{Identifier: "left", State: "Todo", CreatedAt: &earlier}, right: connector.Issue{Identifier: "right", State: "Todo"}},
		{name: "identifier", left: connector.Issue{Identifier: "left", State: "Todo"}, right: connector.Issue{Identifier: "right", State: "Todo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issues := []connector.Issue{tt.right, tt.left}
			sortCandidates(issues, settings)
			if issues[0].Identifier != "left" {
				t.Fatalf("sortCandidates() = %#v", issues)
			}
		})
	}
	issues := []connector.Issue{{Identifier: "right", State: "Todo", CreatedAt: &later}, {Identifier: "left", State: "Todo"}}
	sortCandidates(issues, settings)
	if issues[0].Identifier != "right" {
		t.Fatalf("sortCandidates(nil left date) = %#v", issues)
	}

	if evaluations, err := parseEvaluations("  "); err != nil || evaluations != nil {
		t.Fatalf("parseEvaluations(empty) = %#v, %v", evaluations, err)
	}
	for _, raw := range []string{`{"evaluations":[]}{}`, `{"unknown":true}`} {
		if _, err := parseEvaluations(raw); err == nil {
			t.Fatalf("parseEvaluations(%q) error = nil", raw)
		}
	}
	collector := &proposalCollector{}
	if result, err := collector.handle(t.Context(), runner.AgentToolCall{Name: "other"}); err != nil || result.Success {
		t.Fatalf("collector unsupported = %#v, %v", result, err)
	}
	if result, err := collector.handle(t.Context(), runner.AgentToolCall{Name: ProposalToolName, Arguments: []byte("{")}); err != nil || result.Success {
		t.Fatalf("collector invalid = %#v, %v", result, err)
	}
	if _, err := collector.result(); err == nil {
		t.Fatal("collector result error = nil")
	}

	for _, issue := range []connector.Issue{
		{Identifier: " DD-1 "},
		{URL: " HTTPS://EXAMPLE.COM/1 "},
		{Title: "fallback"},
	} {
		if admissionCandidateKey(issue) == "" {
			t.Fatalf("admissionCandidateKey(%#v) is empty", issue)
		}
	}
	if got := normalizeStrings([]string{"", " Todo ", "todo", "Backlog"}); !reflect.DeepEqual(got, []string{"Todo", "Backlog"}) {
		t.Fatalf("normalizeStrings() = %#v", got)
	}

	criteria := admissionTestCriteria()
	if _, err := validateFindings(nil, criteria); !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("validateFindings(nil) error = %v", err)
	}
	duplicate := admissionmodel.Finding{Dimension: "Alignment", CriterionQuote: "serves a stated current priority", Matched: true, Rationale: "valid"}
	if _, err := validateFindings([]admissionmodel.Finding{duplicate, duplicate}, criteria); !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("validateFindings(duplicate) error = %v", err)
	}
	unmatched := duplicate
	unmatched.Matched = false
	if _, err := validateFindings([]admissionmodel.Finding{unmatched}, criteria); !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("validateFindings(unmatched) error = %v", err)
	}
	complete := []admissionmodel.Finding{
		duplicate,
		{Dimension: "Readiness", CriterionQuote: "has an actionable problem statement", Matched: true, Rationale: "valid"},
	}
	if _, err := validateFindings(complete, criteria); err != nil {
		t.Fatalf("validateFindings(complete) error = %v", err)
	}
	if _, _, err := validateRecommendedEffort("high", "valid", config.AdmissionEffortRubric{}, true); !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("validateRecommendedEffort(required without rubric) error = %v", err)
	}
	if effort, rationale, err := validateRecommendedEffort("high", "valid", config.AdmissionEffortRubric{}, false); err != nil || effort != "high" || rationale != "valid" {
		t.Fatalf("validateRecommendedEffort(optional) = %q, %q, %v", effort, rationale, err)
	}
	if _, _, err := validateRecommendedEffort("high", "valid", config.AdmissionEffortRubric{Efforts: []string{"medium"}}, false); !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("validateRecommendedEffort(unsupported) error = %v", err)
	}

	budgetRunner := &budgetAdmissionRunner{scriptedAdmissionRunner: &scriptedAdmissionRunner{}, status: runner.DailyBudgetStatus{}}
	if deferred, reason, err := admissionBudgetDeferred(t.Context(), budgetRunner, earlier); err != nil || deferred || reason != "" {
		t.Fatalf("admissionBudgetDeferred() = %t, %q, %v", deferred, reason, err)
	}

	cfg := config.BacklogAdmission{TargetState: "Todo"}
	skipped := map[string]int{}
	if filtered := filterCandidates([]connector.Issue{{Closed: true}}, cfg, nil, skipped, false); len(filtered) != 0 || skipped["closed"] != 1 {
		t.Fatalf("filterCandidates(closed) = %#v, skipped = %#v", filtered, skipped)
	}
	if eligibleCandidate(connector.Issue{Closed: true}, cfg, nil) {
		t.Fatal("eligibleCandidate(closed) = true")
	}
	if eligibleCandidate(connector.Issue{State: "Todo"}, cfg, nil) {
		t.Fatal("eligibleCandidate(target state) = true")
	}

	tracker := memory.New(memory.Config{Stateful: true})
	plainTracker := &failingCandidateIssueStore{IssueStore: tracker}
	if transition, found, err := admissionIssueTransition(t.Context(), plainTracker, connector.Issue{}); err != nil || found || !transition.EnteredAt.IsZero() {
		t.Fatalf("admissionIssueTransition() = %#v, %t, %v", transition, found, err)
	}
	identified := &identifiedAdmissionIssueStore{IssueStore: tracker, login: "detent-bot"}
	if decision := automaticAdmissionDecision(identified, admissionmodel.Proposal{ID: "proposal"}, earlier); decision.ActorLogin != "detent-bot" {
		t.Fatalf("automaticAdmissionDecision() actor = %q", decision.ActorLogin)
	}

	proposal := admissionmodel.Proposal{ID: "proposal", CreatedAt: earlier, CommentedAt: earlier.Add(time.Minute)}
	before := earlier
	laterDecision := earlier.Add(3 * time.Minute)
	earlierDecision := earlier.Add(2 * time.Minute)
	decision, found, err := proposalDecision(t.Context(), tracker, connector.Issue{}, proposal, []connector.IssueComment{
		{Body: admissionRejectCommand(proposal.ID), CreatedAt: &before, AuthorAuthorized: true},
		{Body: admissionRejectCommand(proposal.ID), CreatedAt: &laterDecision, AuthorAuthorized: true},
		{Body: admissionRejectCommand(proposal.ID), CreatedAt: &earlierDecision, AuthorAuthorized: true},
	})
	if err != nil || !found || decision.Outcome != admissionmodel.ProposalRejected || !decision.DecidedAt.Equal(laterDecision) {
		t.Fatalf("proposalDecision() = %#v, %t, %v", decision, found, err)
	}

	comment := proposalComment(admissionmodel.Proposal{TargetState: "Todo"}, config.AdmissionCriteria{}, true, true, "")
	if !strings.Contains(comment, "Automatic admission is a two-part change") {
		t.Fatalf("proposalComment(auto admit) = %q", comment)
	}
	var decoded map[string]any
	if err := decodeStrictJSON([]byte(`{} invalid`), &decoded); err == nil {
		t.Fatal("decodeStrictJSON(trailing invalid data) error = nil")
	}
}

func TestManagerFiltersLocallyAndRejectsFabricatedCriteria(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	allowed := admissionIssueFixture("allowed", "DD-1", 1, now)
	allowed.AuthorID = "octocat"
	excluded := admissionIssueFixture("excluded", "DD-2", 2, now)
	excluded.AuthorID = "octocat"
	excluded.Labels = []string{"do-not-admit"}
	otherAuthor := admissionIssueFixture("other", "DD-3", 3, now)
	otherAuthor.AuthorID = "hubot"
	tracker := memory.New(memory.Config{Issues: []connector.Issue{allowed, excluded, otherAuthor}, Stateful: true})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: func(request runner.RunRequest) []AgentProposal {
		return []AgentProposal{{
			IssueID: request.Admission.Candidates[0].ID,
			Findings: []admissionmodel.Finding{{
				Dimension:      "Alignment",
				CriterionQuote: "fabricated criterion",
				Matched:        true,
				Rationale:      "Untrusted issue text claimed a match.",
			}},
			Confidence: float64Pointer(1),
		}}
	}}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.ExcludeLabels = []string{"do-not-admit"}
	settings.Config.Authors.Allow = []string{"octocat"}
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
	result, err := manager.RunOnce(context.Background())
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("RunOnce() error = %v, want ErrInvalidOutput", err)
	}
	if got, want := agent.candidateIDs[0], []string{"allowed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate ids = %#v, want %#v", got, want)
	}
	if len(result.Proposals) != 0 || result.Skipped["excluded_label"] != 1 ||
		result.Skipped["author"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestManagerUnionsLabelCandidatesAndSkipsIneligibleStates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	stateAndLabel := admissionIssueFixture("both", "DD-1", 1, now)
	stateAndLabel.Labels = []string{"sentry"}
	labelOnly := admissionIssueFixture("label", "DD-2", 2, now.Add(time.Minute))
	labelOnly.State = "Needs Triage"
	labelOnly.Labels = []string{"needs-decision"}
	target := admissionIssueFixture("target", "DD-6", 6, now.Add(5*time.Minute))
	target.State = "Todo"
	target.Labels = []string{"sentry"}
	blocked := admissionIssueFixture("blocked", "DD-3", 3, now.Add(2*time.Minute))
	blocked.State = "Blocked"
	blocked.Labels = []string{"sentry"}
	terminal := admissionIssueFixture("terminal", "DD-4", 4, now.Add(3*time.Minute))
	terminal.State = "Done"
	terminal.Labels = []string{"sentry"}
	excluded := admissionIssueFixture("excluded", "DD-5", 5, now.Add(4*time.Minute))
	excluded.Labels = []string{"sentry", "skip"}
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{stateAndLabel, labelOnly, target, blocked, terminal, excluded},
		Stateful: true,
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.Sources.Labels = []string{"sentry", "needs-decision"}
	settings.Config.ExcludeLabels = []string{"skip"}
	settings.Config.MaxProposalsPerRun = 10
	settings.TerminalStates = []string{"Done"}
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.CandidatesFound != 6 || result.Candidates != 2 || len(result.Proposals) != 2 {
		t.Fatalf("result = %#v, want deduplicated union with two eligible candidates", result)
	}
	if result.Skipped["label_target_state"] != 1 ||
		result.Skipped["label_blocked_state"] != 1 ||
		result.Skipped["label_terminal_state"] != 1 ||
		result.Skipped["excluded_label"] != 1 {
		t.Fatalf("skipped = %#v", result.Skipped)
	}
	if got, want := agent.candidateIDs[0], []string{"both", "label"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate ids = %#v, want %#v", got, want)
	}
}

func TestAllowedAuthorUnion(t *testing.T) {
	t.Parallel()

	authors := config.BacklogAdmissionAuthors{
		Allow:            []string{"octocat"},
		AllowAssociation: []string{"MEMBER"},
	}
	tests := []struct {
		name  string
		issue connector.Issue
		want  bool
	}{
		{name: "matching handle", issue: connector.Issue{AuthorID: "@octocat", AuthorAssociation: connector.AuthorAssociationNone}, want: true},
		{name: "matching association", issue: connector.Issue{AuthorID: "hubot", AuthorAssociation: connector.AuthorAssociationMember}, want: true},
		{name: "missing association", issue: connector.Issue{AuthorID: "hubot"}},
		{name: "no match", issue: connector.Issue{AuthorID: "hubot", AuthorAssociation: connector.AuthorAssociationContributor}},
		{name: "unrestricted default", issue: connector.Issue{}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configured := authors
			if test.name == "unrestricted default" {
				configured = config.BacklogAdmissionAuthors{}
			}
			if got := allowedAuthor(test.issue, configured); got != test.want {
				t.Fatalf("allowedAuthor() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestManagerAuthorRejectionIsAggregateOnly(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	outsider := admissionIssueFixture("outsider", "DD-1", 1, now)
	outsider.AuthorID = "stranger"
	outsider.AuthorAssociation = connector.AuthorAssociationNone
	tracker := memory.New(memory.Config{Issues: []connector.Issue{outsider}, Stateful: true})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.Authors.AllowAssociation = []string{"MEMBER"}
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Skipped["author"] != 1 || agent.calls != 0 {
		t.Fatalf("result = %#v, runner calls = %d", result, agent.calls)
	}
	record, ok, err := backend.LatestAdmissionRun(context.Background(), "detent")
	if err != nil || !ok || record.Skipped["author"] != 1 {
		t.Fatalf("LatestAdmissionRun() = %#v, %t, %v", record, ok, err)
	}
	for _, event := range tracker.Events() {
		if event.Kind == memory.EventKindComment {
			t.Fatalf("author rejection created a comment: %#v", event)
		}
	}
}

func TestManagerUsesAuthorPushdownOnlyWhenUnionSafe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name              string
		allowAssociation  []string
		wantPushedAuthors []string
		filtered          map[string]int
	}{
		{name: "handle only", wantPushedAuthors: []string{"octocat"}, filtered: map[string]int{"author": 2}},
		{name: "association union", allowAssociation: []string{"MEMBER"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			issue := admissionIssueFixture("candidate", "DD-1", 1, now)
			issue.AuthorID = "octocat"
			issue.AuthorAssociation = connector.AuthorAssociationMember
			memoryStore := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
			tracker := &recordingCandidateIssueStore{
				IssueStore:   memoryStore,
				capabilities: connector.CandidateCapabilitiesFor(connector.BackendGitHub, "issue_field"),
				filtered:     test.filtered,
			}
			settings := admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate})
			settings.Config.Authors.Allow = []string{"octocat"}
			settings.Config.Authors.AllowAssociation = test.allowAssociation
			manager := newAdmissionTestManager(t, settings, openManagerTestStore(t), func() time.Time { return now })

			result, err := manager.RunOnce(context.Background())
			if err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if len(tracker.requests) != 1 || !reflect.DeepEqual(tracker.requests[0].Authors, test.wantPushedAuthors) {
				t.Fatalf("candidate requests = %#v, want authors %#v", tracker.requests, test.wantPushedAuthors)
			}
			if result.Skipped["author"] != test.filtered["author"] {
				t.Fatalf("skipped authors = %d, want %d", result.Skipped["author"], test.filtered["author"])
			}
		})
	}
}

func TestReadAdmissionCandidatesPushesAuthorsOnlyToStates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("candidate", "DD-1", 1, now)
	issue.AuthorID = "octocat"
	issue.Labels = []string{"sentry"}
	memoryStore := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	tracker := &recordingCandidateIssueStore{
		IssueStore:   memoryStore,
		capabilities: connector.CandidateCapabilitiesFor(connector.BackendGitHub, "issue_field"),
	}
	cfg := admissionTestSettings(memoryStore, &scriptedAdmissionRunner{}).Config
	cfg.Sources.Labels = []string{"sentry"}
	cfg.Authors.Allow = []string{"octocat"}

	_, _, _, _, err := readAdmissionCandidates(context.Background(), tracker, cfg)
	if err != nil {
		t.Fatalf("readAdmissionCandidates() error = %v", err)
	}
	if len(tracker.requests) != 2 {
		t.Fatalf("candidate requests = %#v, want states and labels", tracker.requests)
	}
	if got := tracker.requests[0].Authors; !reflect.DeepEqual(got, []string{"octocat"}) {
		t.Fatalf("states authors = %#v, want octocat", got)
	}
	if got := tracker.requests[1].Authors; len(got) != 0 {
		t.Fatalf("labels authors = %#v, want local filtering", got)
	}
}

func TestManagerDeduplicatesPushedAuthorRejectionsAcrossSelectors(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	rejected := admissionIssueFixture("candidate", "DD-1", 1, now)
	rejected.AuthorID = "outsider"
	rejected.Labels = []string{"candidate"}
	memoryStore := memory.New(memory.Config{Issues: []connector.Issue{rejected}, Stateful: true})
	issues := &selectorAdmissionIssueStore{
		IssueStore:   memoryStore,
		capabilities: connector.CandidateCapabilitiesFor(connector.BackendGitHub, "issue_field"),
		results: map[connector.CandidateSelector]connector.CandidateResult{
			connector.CandidateSelectorStates: {
				Filtered: map[string]int{"author": 1},
			},
			connector.CandidateSelectorLabels: {
				Issues: []connector.Issue{rejected},
			},
		},
	}
	settings := admissionTestSettings(issues, &scriptedAdmissionRunner{propose: proposeEveryCandidate})
	settings.Config.Sources.Labels = []string{"candidate"}
	settings.Config.Authors.Allow = []string{"trusted"}
	manager := newAdmissionTestManager(t, settings, openManagerTestStore(t), func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.Skipped["author"] != 1 {
		t.Fatalf("skipped authors = %d, want one deduplicated rejection", result.Skipped["author"])
	}
	if result.Candidates != 0 || len(result.Proposals) != 0 {
		t.Fatalf("result = %#v, want no eligible candidates", result)
	}
}

func TestManagerUnionsUntrackedCandidatesBeforeDeduplicationAndExclusions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracked := admissionIssueFixture("tracked", "DD-1", 1, now)
	duplicate := tracked
	duplicate.State = ""
	untracked := admissionIssueFixture("untracked", "DD-2", 2, now.Add(time.Minute))
	untracked.State = ""
	excluded := admissionIssueFixture("excluded", "DD-3", 3, now.Add(2*time.Minute))
	excluded.State = ""
	excluded.Labels = []string{"do-not-admit"}
	tracker := memory.New(memory.Config{
		Issues:   []connector.Issue{tracked, untracked, excluded},
		Stateful: true,
		Now:      func() time.Time { return now },
	})
	issues := &selectorAdmissionIssueStore{
		IssueStore: tracker,
		results: map[connector.CandidateSelector]connector.CandidateResult{
			connector.CandidateSelectorStates: {
				Issues: []connector.Issue{tracked},
			},
			connector.CandidateSelectorUntracked: {
				Issues:    []connector.Issue{duplicate, untracked, excluded},
				Truncated: true,
			},
		},
	}
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(issues, agent)
	settings.Config.Sources.Untracked = true
	settings.Config.ExcludeLabels = []string{"do-not-admit"}
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if result.CandidatesFound != 3 || result.Candidates != 2 || len(result.Proposals) != 2 {
		t.Fatalf("result = %#v, want three union candidates and two proposals", result)
	}
	if result.Skipped["excluded_label"] != 1 || result.Truncated["candidate_reader"] != 1 {
		t.Fatalf("result filters and truncation = %#v", result)
	}
	if len(issues.requests) != 2 ||
		issues.requests[0].Selector != connector.CandidateSelectorStates ||
		issues.requests[1].Selector != connector.CandidateSelectorUntracked {
		t.Fatalf("candidate requests = %#v, want states then untracked", issues.requests)
	}

	comments := map[string]string{}
	for _, event := range tracker.Events() {
		if event.Kind == memory.EventKindComment {
			comments[event.IssueID] = event.Body
		}
	}
	if strings.Contains(comments["tracked"], "Acceptance is a two-part change") {
		t.Fatalf("tracked proposal used untracked wording: %s", comments["tracked"])
	}
	for _, want := range []string{"no configured status label", "Acceptance is a two-part change", "admitting the work for dispatch"} {
		if !strings.Contains(comments["untracked"], want) {
			t.Fatalf("untracked proposal missing %q: %s", want, comments["untracked"])
		}
	}
}

func TestManagerLocalSQLiteStatesOnlyEndToEnd(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker, err := local.New(local.Config{
		Path:           filepath.Join(t.TempDir(), "tracker.db"),
		ProjectID:      "detent",
		Issues:         []connector.Issue{admissionIssueFixture("local-1", "LOCAL-1", 1, now)},
		ObservedStates: []string{"Backlog"},
		ActiveStates:   []string{"Todo"},
		TerminalStates: []string{"Done"},
	})
	if err != nil {
		t.Fatalf("local.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := tracker.Close(); err != nil {
			t.Fatalf("tracker.Close() error = %v", err)
		}
	})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.ExcludeLabels = nil
	settings.Config.Authors.Allow = nil
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })
	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(result.Proposals) != 1 || result.Proposals[0].IssueID != "local-1" {
		t.Fatalf("result = %#v", result)
	}
	issues, err := tracker.FetchIssuesByStates(context.Background(), []string{"Backlog"})
	if err != nil || len(issues) != 1 || len(issues[0].Comments) != 1 {
		t.Fatalf("persisted tracker issue = %#v, %v", issues, err)
	}
}

func TestManagerReservesCommentCapacityBeforeCreatingProposals(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	uncommented := admissionIssueFixture("issue-0", "DD-0", 1, now.Add(-time.Hour))
	candidate := admissionIssueFixture("issue-1", "DD-1", 2, now)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{uncommented, candidate}, Stateful: true})
	backend := openManagerTestStore(t)
	created, err := backend.CreateAdmissionProposal(context.Background(), admissionmodel.Proposal{
		ID:              "admission-uncommented",
		ProjectID:       "detent",
		IssueID:         uncommented.ID,
		IssueIdentifier: uncommented.Identifier,
		TargetState:     "Todo",
		Fingerprint:     issueFingerprint(uncommented),
		CriteriaSection: "Admission criteria",
		CriteriaText:    admissionTestCriteria().Text,
		Findings: []admissionmodel.Finding{{
			Dimension:      "Alignment",
			CriterionQuote: "serves a stated current priority",
			Matched:        true,
			Rationale:      "Supports the priority.",
		}},
		Confidence: 0.8,
		Status:     admissionmodel.ProposalOpen,
		CreatedAt:  now.Add(-time.Hour),
		ExpiresAt:  now.AddDate(0, 0, 7),
	})
	if err != nil || !created {
		t.Fatalf("CreateAdmissionProposal() = %v, %v", created, err)
	}
	agent := &scriptedAdmissionRunner{propose: proposeEveryCandidate}
	settings := admissionTestSettings(tracker, agent)
	settings.Config.MaxProposalsPerRun = 1
	manager := newAdmissionTestManager(t, settings, backend, func() time.Time { return now })

	result, err := manager.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if agent.calls != 0 || result.Skipped["comment_cap"] != 1 || len(result.Proposals) != 0 {
		t.Fatalf("result = %#v, runner calls = %d", result, agent.calls)
	}
	open, err := backend.OpenAdmissionProposals(context.Background(), "detent", 0)
	if err != nil || len(open) != 1 || open[0].CommentedAt.IsZero() {
		t.Fatalf("open proposals = %#v, %v", open, err)
	}
}

func TestManagerRejectsMissingConfidence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{admissionIssueFixture("issue-1", "DD-1", 1, now)}, Stateful: true})
	backend := openManagerTestStore(t)
	agent := &scriptedAdmissionRunner{propose: func(request runner.RunRequest) []AgentProposal {
		return []AgentProposal{{
			IssueID: request.Admission.Candidates[0].ID,
			Findings: []admissionmodel.Finding{{
				Dimension:      "Alignment",
				CriterionQuote: "serves a stated current priority",
				Matched:        true,
				Rationale:      "Supports the priority.",
			}},
		}}
	}}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return now })
	result, err := manager.RunOnce(context.Background())
	if !errors.Is(err, ErrInvalidOutput) || len(result.Proposals) != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestManagerPersistsDeclinedCandidateEvaluation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	issue := admissionIssueFixture("issue-1", "DD-1", 1, now)
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})
	backend := openManagerTestStore(t)
	agent := staticAdmissionRunner{output: `{"evaluations":[{"issue_id":"issue-1","disposition":"declined","findings":[{"dimension":"Alignment","criterion_quote":"serves a stated current priority","matched":false,"rationale":"The issue does not serve a stated current priority."},{"dimension":"Readiness","criterion_quote":"has an actionable problem statement","matched":false,"rationale":"The issue lacks an actionable problem statement."}],"confidence":0.24}]}`}
	manager := newAdmissionTestManager(t, admissionTestSettings(tracker, agent), backend, func() time.Time { return now })

	result, err := manager.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(result.Proposals) != 0 || result.Skipped[admissionDeclineCriteriaNotMet] != 1 ||
		result.ProposalReason != admissionDeclineCriteriaNotMet {
		t.Fatalf("result = %#v", result)
	}
	decline, found, err := backend.AdmissionDecline(
		t.Context(),
		"detent",
		issue.ID,
		criteriaDeclineFingerprint(issue, admissionTestCriteria()),
	)
	if err != nil || !found {
		t.Fatalf("AdmissionDecline() = %#v, %t, %v", decline, found, err)
	}
	if decline.IssueIdentifier != issue.Identifier || decline.Reason != admissionDeclineCriteriaNotMet ||
		decline.Confidence == nil || *decline.Confidence != 0.24 ||
		decline.FailedDimension != "Alignment" ||
		decline.FailedCriterion != "serves a stated current priority" {
		t.Fatalf("decline = %#v", decline)
	}
	record, found, err := backend.LatestAdmissionRun(t.Context(), "detent")
	if err != nil || !found || record.ProposalReason != admissionDeclineCriteriaNotMet ||
		record.Candidates != 1 || record.Proposed != 0 {
		t.Fatalf("LatestAdmissionRun() = %#v, %t, %v", record, found, err)
	}
}

func TestManagerRequiresOneEvaluationPerCandidate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		evaluations []AgentEvaluation
	}{
		{
			name:        "missing candidate",
			evaluations: []AgentEvaluation{admissionProposalEvaluation("issue-1")},
		},
		{
			name: "duplicate candidate",
			evaluations: []AgentEvaluation{
				admissionProposalEvaluation("issue-1"),
				admissionProposalEvaluation("issue-1"),
			},
		},
		{
			name: "unknown candidate",
			evaluations: []AgentEvaluation{
				admissionProposalEvaluation("issue-1"),
				admissionProposalEvaluation("unknown"),
			},
		},
		{
			name: "invalid disposition",
			evaluations: []AgentEvaluation{
				admissionProposalEvaluation("issue-1"),
				admissionProposalEvaluation("issue-2"),
			},
		},
	}
	tests[3].evaluations[1].Disposition = "pending"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issues := []connector.Issue{
				admissionIssueFixture("issue-1", "DD-1", 1, now),
				admissionIssueFixture("issue-2", "DD-2", 2, now),
			}
			tracker := memory.New(memory.Config{Issues: issues, Stateful: true})
			backend := openManagerTestStore(t)
			raw, err := json.Marshal(struct {
				Evaluations []AgentEvaluation `json:"evaluations"`
			}{Evaluations: tt.evaluations})
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			manager := newAdmissionTestManager(
				t,
				admissionTestSettings(tracker, staticAdmissionRunner{output: string(raw)}),
				backend,
				func() time.Time { return now },
			)

			result, err := manager.RunOnce(t.Context())
			if !errors.Is(err, ErrInvalidOutput) || result.Candidates != len(issues) || len(result.Proposals) != 0 {
				t.Fatalf("RunOnce() = %#v, %v", result, err)
			}
		})
	}
}

func TestManagerValidatesEntireEvaluationBatchBeforeSideEffects(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	issues := []connector.Issue{
		admissionIssueFixture("issue-1", "DD-1", 1, now),
		admissionIssueFixture("issue-2", "DD-2", 2, now),
	}
	evaluations := []AgentEvaluation{
		admissionProposalEvaluation(issues[0].ID),
		admissionProposalEvaluation(issues[1].ID),
	}
	evaluations[1].Confidence = float64Pointer(2)
	raw, err := json.Marshal(struct {
		Evaluations []AgentEvaluation `json:"evaluations"`
	}{Evaluations: evaluations})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	tracker := memory.New(memory.Config{Issues: issues, Stateful: true})
	backend := openManagerTestStore(t)
	manager := newAdmissionTestManager(
		t,
		admissionTestSettings(tracker, staticAdmissionRunner{output: string(raw)}),
		backend,
		func() time.Time { return now },
	)

	result, err := manager.RunOnce(t.Context())
	if !errors.Is(err, ErrInvalidOutput) || len(result.Proposals) != 0 {
		t.Fatalf("RunOnce() = %#v, %v", result, err)
	}
	for _, issue := range issues {
		history, historyErr := backend.AdmissionProposalHistory(t.Context(), "detent", issue.ID)
		if historyErr != nil || len(history) != 0 {
			t.Fatalf("AdmissionProposalHistory(%s) = %#v, %v", issue.ID, history, historyErr)
		}
	}
}

func TestManagerSkipsCandidateChangedDuringEvaluationAndContinues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	first := admissionIssueFixture("issue-1", "DD-1", 1, now)
	second := admissionIssueFixture("issue-2", "DD-2", 2, now)
	tracker := &stateChangingAdmissionStore{
		Connector: memory.New(memory.Config{Issues: []connector.Issue{first, second}, Stateful: true}),
		fetches:   1,
		issueID:   first.ID,
		state:     "In Progress",
	}
	backend := openManagerTestStore(t)
	manager := newAdmissionTestManager(
		t,
		admissionTestSettings(tracker, &scriptedAdmissionRunner{propose: proposeEveryCandidate}),
		backend,
		func() time.Time { return now },
	)

	result, err := manager.RunOnce(t.Context())
	if err != nil || result.Skipped["stale_or_ineligible"] != 1 ||
		len(result.Proposals) != 1 || result.Proposals[0].IssueID != second.ID {
		t.Fatalf("RunOnce() = %#v, %v", result, err)
	}
}

func TestParseEvaluationsRequiresTypedEnvelope(t *testing.T) {
	t.Parallel()

	if _, err := parseEvaluations(`{}`); err == nil {
		t.Fatal("parseEvaluations({}) error = nil")
	}
	evaluations, err := parseEvaluations(`{"evaluations":[]}`)
	if err != nil || len(evaluations) != 0 {
		t.Fatalf("parseEvaluations(empty) = %#v, %v", evaluations, err)
	}
}

func TestProposalToolRequiresEffortFieldsOnlyWhenConfigured(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		requireEffort bool
		wantRequired  bool
	}{
		{name: "optional by default"},
		{name: "required when configured", requireEffort: true, wantRequired: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var schema struct {
				Required []string `json:"required"`
				AllOf    []struct {
					Then struct {
						Required []string `json:"required"`
					} `json:"then"`
				} `json:"allOf"`
			}
			if err := json.Unmarshal(proposalTool(tt.requireEffort).InputSchema, &schema); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			required := map[string]struct{}{}
			for _, field := range schema.Required {
				required[field] = struct{}{}
			}
			for _, condition := range schema.AllOf {
				for _, field := range condition.Then.Required {
					required[field] = struct{}{}
				}
			}
			_, effortRequired := required["recommended_effort"]
			_, rationaleRequired := required["effort_rationale"]
			if effortRequired != tt.wantRequired || rationaleRequired != tt.wantRequired {
				t.Fatalf("required fields = %#v", schema.Required)
			}
		})
	}
}

type scriptedAdmissionRunner struct {
	propose      func(runner.RunRequest) []AgentProposal
	calls        int
	candidateIDs [][]string
}

type staticAdmissionRunner struct {
	output string
}

func (r staticAdmissionRunner) Run(context.Context, runner.RunRequest) (runner.RunResult, error) {
	return runner.RunResult{Output: r.output, FinalState: runner.FinalStateCompleted}, nil
}

type capacityScheduler struct {
	requestErr error
	releaseErr error
	releases   int
}

func (s *capacityScheduler) RequestSlot(context.Context, scheduler.SlotRequest) (scheduler.Slot, error) {
	return scheduler.Slot{}, s.requestErr
}

func (s *capacityScheduler) ReleaseSlot(scheduler.Slot) error {
	s.releases++
	return s.releaseErr
}

func (*capacityScheduler) Mode() scheduler.Mode {
	return scheduler.ModeCountingSemaphore
}

type capacityGate struct {
	acquired   bool
	tryErr     error
	releaseErr error
	markedIdle []scheduler.ProjectCandidate
	releases   int
}

func (*capacityGate) MarkReady(scheduler.ProjectCandidate) {}

func (g *capacityGate) MarkIdle(candidate scheduler.ProjectCandidate) {
	g.markedIdle = append(g.markedIdle, candidate)
}

func (g *capacityGate) TryAcquire(
	context.Context,
	scheduler.ProjectCandidate,
	scheduler.SlotRequest,
	time.Time,
) (scheduler.Slot, bool, error) {
	return scheduler.Slot{}, g.acquired, g.tryErr
}

func (*capacityGate) SetPreempt(scheduler.Slot, func()) {}

func (g *capacityGate) Release(scheduler.Slot) error {
	g.releases++
	return g.releaseErr
}

type faultAdmissionStore struct {
	Store
	expireErr             error
	refreshErr            error
	recordErr             error
	latestErr             error
	targetTransitionErr   error
	declineCommentMarkErr error
	createDeclineErr      error
	declineReadErr        error
	declineMissing        bool
	latest                admissionmodel.RunRecord
	latestOK              bool
}

type admissionIssueStoreWithoutCommentReader struct {
	IssueStore
}

func (s *faultAdmissionStore) CreateAdmissionDecline(ctx context.Context, decline admissionmodel.Decline) (bool, error) {
	if s.createDeclineErr != nil {
		err := s.createDeclineErr
		s.createDeclineErr = nil
		return false, err
	}
	return s.Store.CreateAdmissionDecline(ctx, decline)
}

func (s *faultAdmissionStore) AdmissionDecline(
	ctx context.Context,
	projectID string,
	issueID string,
	fingerprint string,
) (admissionmodel.Decline, bool, error) {
	if s.declineReadErr != nil {
		err := s.declineReadErr
		s.declineReadErr = nil
		return admissionmodel.Decline{}, false, err
	}
	if s.declineMissing {
		s.declineMissing = false
		return admissionmodel.Decline{}, false, nil
	}
	return s.Store.AdmissionDecline(ctx, projectID, issueID, fingerprint)
}

func (s *faultAdmissionStore) ExpireAdmissionProposals(ctx context.Context, projectID string, cutoff time.Time) (int, error) {
	if s.expireErr != nil {
		return 0, s.expireErr
	}
	return s.Store.ExpireAdmissionProposals(ctx, projectID, cutoff)
}

func (s *faultAdmissionStore) RefreshAdmissionOutcomes(ctx context.Context, refresh admissionmodel.OutcomeRefresh) error {
	if s.refreshErr != nil {
		return s.refreshErr
	}
	return s.Store.RefreshAdmissionOutcomes(ctx, refresh)
}

func (s *faultAdmissionStore) RecordAdmissionRun(ctx context.Context, record admissionmodel.RunRecord) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	return s.Store.RecordAdmissionRun(ctx, record)
}

func (s *faultAdmissionStore) LatestAdmissionRun(ctx context.Context, projectID string) (admissionmodel.RunRecord, bool, error) {
	if s.latestErr != nil || s.latestOK {
		return s.latest, s.latestOK, s.latestErr
	}
	return s.Store.LatestAdmissionRun(ctx, projectID)
}

func (s *faultAdmissionStore) AdmissionTargetTransitions(
	ctx context.Context,
	query admissionmodel.TargetTransitionQuery,
) ([]admissionmodel.TargetTransition, error) {
	if s.targetTransitionErr != nil {
		return nil, s.targetTransitionErr
	}
	return s.Store.AdmissionTargetTransitions(ctx, query)
}

func (s *faultAdmissionStore) MarkAdmissionDeclineCommented(ctx context.Context, id string, at time.Time) error {
	if s.declineCommentMarkErr != nil {
		err := s.declineCommentMarkErr
		s.declineCommentMarkErr = nil
		return err
	}
	return s.Store.MarkAdmissionDeclineCommented(ctx, id, at)
}

type boundaryBudgetRunner struct {
	runner.Backend
	err error
}

func (r *boundaryBudgetRunner) DailyBudgetStatus(context.Context, time.Time) (runner.DailyBudgetStatus, bool, error) {
	return runner.DailyBudgetStatus{}, false, r.err
}

type failingCandidateIssueStore struct {
	IssueStore
	err error
}

func (s *failingCandidateIssueStore) ReadCandidates(context.Context, connector.CandidateRequest) (connector.CandidateResult, error) {
	return connector.CandidateResult{}, s.err
}

type identifiedAdmissionIssueStore struct {
	IssueStore
	login string
}

func (s *identifiedAdmissionIssueStore) InstanceLogin() string {
	return s.login
}

type selectorAdmissionIssueStore struct {
	IssueStore
	capabilities connector.CandidateCapabilities
	results      map[connector.CandidateSelector]connector.CandidateResult
	requests     []connector.CandidateRequest
}

func (s *selectorAdmissionIssueStore) CandidateCapabilities() connector.CandidateCapabilities {
	if len(s.capabilities.Selectors) > 0 {
		return s.capabilities
	}
	return s.IssueStore.CandidateCapabilities()
}

func (s *selectorAdmissionIssueStore) ReadCandidates(
	_ context.Context,
	request connector.CandidateRequest,
) (connector.CandidateResult, error) {
	s.requests = append(s.requests, request)
	return s.results[request.Selector], nil
}

func (r *scriptedAdmissionRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	r.calls++
	ids := make([]string, 0, len(request.Admission.Candidates))
	for _, candidate := range request.Admission.Candidates {
		ids = append(ids, candidate.ID)
	}
	r.candidateIDs = append(r.candidateIDs, ids)
	for _, proposal := range r.propose(request) {
		if proposal.Disposition == "" {
			proposal.Disposition = admissionDispositionProposed
		}
		raw, err := json.Marshal(proposal)
		if err != nil {
			return runner.RunResult{}, err
		}
		result, err := request.AgentToolHandler(ctx, runner.AgentToolCall{Name: ProposalToolName, Arguments: raw})
		if err != nil {
			return runner.RunResult{}, err
		}
		if !result.Success {
			return runner.RunResult{}, ErrInvalidOutput
		}
	}
	return runner.RunResult{FinalState: runner.FinalStateCompleted}, nil
}

type budgetAdmissionRunner struct {
	*scriptedAdmissionRunner
	status runner.DailyBudgetStatus
}

type stateChangingAdmissionStore struct {
	*memory.Connector
	fetches int
	issueID string
	state   string
}

type transitionAdmissionIssueStore struct {
	*memory.Connector
	transition connector.IssueStateTransition
	found      bool
}

func (s *transitionAdmissionIssueStore) IssueStateTransition(
	context.Context,
	connector.Issue,
) (connector.IssueStateTransition, bool, error) {
	return s.transition, s.found, nil
}

type authorizingAdmissionIssueStore struct {
	*memory.Connector
	authorized bool
}

func (s *authorizingAdmissionIssueStore) IsIssueCommentAuthorAuthorized(
	context.Context,
	connector.Issue,
	connector.IssueComment,
) (bool, error) {
	return s.authorized, nil
}

var errAdmissionResolutionFailure = errors.New("admission resolution failed")

type failingAdmissionResolutionStore struct {
	Store
	fail bool
}

func (s *failingAdmissionResolutionStore) ResolveAdmissionProposal(
	ctx context.Context,
	decision admissionmodel.Decision,
) error {
	if s.fail && decision.Automatic && decision.Outcome == admissionmodel.ProposalAccepted {
		s.fail = false
		return errAdmissionResolutionFailure
	}
	return s.Store.ResolveAdmissionProposal(ctx, decision)
}

func (s *stateChangingAdmissionStore) FetchIssueStatesByIDs(
	ctx context.Context,
	issueIDs []string,
) ([]connector.Issue, error) {
	s.fetches++
	if s.fetches == 2 {
		if err := s.UpdateIssueState(ctx, s.issueID, s.state); err != nil {
			return nil, err
		}
	}
	return s.Connector.FetchIssueStatesByIDs(ctx, issueIDs)
}

type recordingCandidateIssueStore struct {
	IssueStore
	capabilities connector.CandidateCapabilities
	requests     []connector.CandidateRequest
	filtered     map[string]int
}

func (s *recordingCandidateIssueStore) CandidateCapabilities() connector.CandidateCapabilities {
	return s.capabilities
}

func (s *recordingCandidateIssueStore) ReadCandidates(
	ctx context.Context,
	request connector.CandidateRequest,
) (connector.CandidateResult, error) {
	s.requests = append(s.requests, request)
	request.Authors = nil
	result, err := s.IssueStore.ReadCandidates(ctx, request)
	result.Filtered = s.filtered
	return result, err
}

func (r *budgetAdmissionRunner) DailyBudgetStatus(context.Context, time.Time) (runner.DailyBudgetStatus, bool, error) {
	return r.status, true, nil
}

func proposeEveryCandidate(request runner.RunRequest) []AgentProposal {
	return proposeEveryCandidateAtConfidence(0.8)(request)
}

func admissionProposalEvaluation(issueID string) AgentEvaluation {
	return AgentEvaluation{
		IssueID:     issueID,
		Disposition: admissionDispositionProposed,
		Findings: []admissionmodel.Finding{
			{
				Dimension:      "Alignment",
				CriterionQuote: "serves a stated current priority",
				Matched:        true,
				Rationale:      "The issue directly supports the stated priority.",
			},
			{
				Dimension:      "Readiness",
				CriterionQuote: "has an actionable problem statement",
				Matched:        true,
				Rationale:      "The issue has an actionable problem statement.",
			},
		},
		Confidence: float64Pointer(0.8),
	}
}

func proposeEveryCandidateAtConfidence(confidence float64) func(runner.RunRequest) []AgentProposal {
	return func(request runner.RunRequest) []AgentProposal {
		proposals := make([]AgentProposal, 0, len(request.Admission.Candidates))
		for _, candidate := range request.Admission.Candidates {
			proposals = append(proposals, AgentProposal{
				IssueID:     candidate.ID,
				Disposition: admissionDispositionProposed,
				Findings: []admissionmodel.Finding{
					{
						Dimension:      "Alignment",
						CriterionQuote: "serves a stated current priority",
						Matched:        true,
						Rationale:      "The issue directly supports the stated priority.",
					},
					{
						Dimension:      "Readiness",
						CriterionQuote: "has an actionable problem statement",
						Matched:        true,
						Rationale:      "The issue has an actionable problem statement.",
					},
				},
				Confidence: float64Pointer(confidence),
			})
		}
		return proposals
	}
}

func proposeEveryCandidateWithEffort(effort string) func(runner.RunRequest) []AgentProposal {
	return func(request runner.RunRequest) []AgentProposal {
		proposals := proposeEveryCandidateAtConfidence(1)(request)
		for index := range proposals {
			proposals[index].RecommendedEffort = effort
			proposals[index].EffortRationale = "The project rubric assigns this effort."
		}
		return proposals
	}
}

func admissionTestSettings(tracker IssueStore, backend runner.Backend) Settings {
	cfg := config.BacklogAdmission{
		Enabled:             true,
		Schedule:            "0 6 * * 1-5",
		Sources:             config.BacklogAdmissionSources{States: []string{"Backlog"}},
		TargetState:         "Todo",
		CriteriaSection:     "Admission criteria",
		MaxCandidatesPerRun: 50,
		MaxProposalsPerRun:  3,
		MaxOpenProposals:    10,
		ProposalExpiryDays:  7,
	}
	return Settings{
		ProjectID:      "detent",
		Config:         cfg,
		Criteria:       admissionTestCriteria(),
		DispatchStates: []string{"Backlog"},
		Runner:         backend,
		Issues:         tracker,
	}
}

func admissionTestEffortRubric() config.AdmissionEffortRubric {
	return config.AdmissionEffortRubric{
		Section: "Issue effort selection",
		Text:    "- `medium` — small and mechanical.\n- `high` — standard feature work.\n- `xhigh` — tricky state semantics.",
		Efforts: []string{"medium", "high", "xhigh"},
	}
}

func float64Pointer(value float64) *float64 {
	return &value
}

func admissionTestCriteria() config.AdmissionCriteria {
	return config.AdmissionCriteria{
		Section: "Admission criteria",
		Text:    "- **Alignment** — serves a stated current priority.\n- **Readiness** — has an actionable problem statement.",
		Dimensions: []config.AdmissionDimension{
			{Name: "Alignment", Text: "- **Alignment** — serves a stated current priority."},
			{Name: "Readiness", Text: "- **Readiness** — has an actionable problem statement."},
		},
	}
}

func admissionTestProposalForIssue(id string, issue connector.Issue, now time.Time) admissionmodel.Proposal {
	return admissionmodel.Proposal{
		ID:              id,
		ProjectID:       "detent",
		IssueID:         issue.ID,
		IssueIdentifier: issue.Identifier,
		TargetState:     "Todo",
		Fingerprint:     issueFingerprint(issue),
		CriteriaSection: "Admission criteria",
		CriteriaText:    admissionTestCriteria().Text,
		Findings: []admissionmodel.Finding{
			{
				Dimension:      "Alignment",
				CriterionQuote: "serves a stated current priority",
				Matched:        true,
				Rationale:      "Supports the priority.",
			},
			{
				Dimension:      "Readiness",
				CriterionQuote: "has an actionable problem statement",
				Matched:        true,
				Rationale:      "The issue has an actionable problem statement.",
			},
		},
		Confidence: 0.8,
		Status:     admissionmodel.ProposalOpen,
		CreatedAt:  now,
		ExpiresAt:  now.AddDate(0, 0, 7),
	}
}

func admissionIssueFixture(id string, identifier string, priority int, createdAt time.Time) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = identifier
	issue.Title = "Candidate " + identifier
	issue.Description = "Actionable problem statement for " + identifier
	issue.State = "Backlog"
	issue.Priority = &priority
	issue.CreatedAt = &createdAt
	return issue
}

func openManagerTestStore(t *testing.T) store.Store {
	t.Helper()
	backend, err := store.Open(context.Background(), store.Config{
		Backend: store.BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "runtime.db"),
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("backend.Close() error = %v", err)
		}
	})
	return backend
}

func newAdmissionTestManager(t *testing.T, settings Settings, backend Store, now func() time.Time) *Manager {
	t.Helper()
	manager, err := New(settings, backend, nil, now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

type staticAdmissionDispatchWorkspace struct {
	path string
}

func (w *staticAdmissionDispatchWorkspace) Create(context.Context, workspace.Issue) (workspace.Info, error) {
	return workspace.Info{Path: w.path, Key: "issue-1", Branch: "detent/issue-1"}, nil
}

func (*staticAdmissionDispatchWorkspace) Cleanup(context.Context, string) error {
	return nil
}

func (*staticAdmissionDispatchWorkspace) BeforeRun(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func (*staticAdmissionDispatchWorkspace) AfterRun(context.Context, workspace.Info, workspace.Issue) {}

func (*staticAdmissionDispatchWorkspace) DiffStat(context.Context, workspace.Info, workspace.Issue) (workspace.DiffStat, error) {
	return workspace.DiffStat{}, nil
}

type effortRecordingAgentBackend struct {
	request runner.AgentTurnRequest
}

func (b *effortRecordingAgentBackend) RunTurn(
	_ context.Context,
	request runner.AgentTurnRequest,
	_ runner.AgentUpdateHandler,
) (runner.AgentTurnResult, error) {
	b.request = request
	return runner.AgentTurnResult{ThreadID: "thread-1", TurnID: "turn-1", SessionID: "session-1"}, nil
}

func (*effortRecordingAgentBackend) ListModels(context.Context) ([]runner.AgentModel, error) {
	return []runner.AgentModel{{
		ID:                        "gpt-default",
		Model:                     "gpt-default",
		Default:                   true,
		SupportedReasoningEfforts: []string{"medium", "high", "xhigh"},
	}}, nil
}

func (*effortRecordingAgentBackend) DefaultModel(context.Context, string) (string, error) {
	return "gpt-default", nil
}

func TestIssueFingerprintIgnoresAuditCommentMetadata(t *testing.T) {
	t.Parallel()

	issue := admissionIssueFixture("issue-1", "DD-1", 1, time.Now())
	before := issueFingerprint(issue)
	updated := time.Now().Add(time.Hour)
	issue.UpdatedAt = &updated
	issue.Comments = []connector.IssueComment{{Body: "audit comment"}}
	after := issueFingerprint(issue)
	if before != after {
		t.Fatalf("fingerprint changed from %q to %q after comment metadata", before, after)
	}
	issue.Description += " changed"
	if issueFingerprint(issue) == after {
		t.Fatal("fingerprint did not change after body edit")
	}
}

func TestClassifyNonDeliverable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		title      string
		body       string
		wantReason string
	}{
		{
			name:       "master tracker title",
			title:      "PyroApex Platform Architecture — Master Tracker",
			body:       "Coordinates work across linked issues.",
			wantReason: admissionDeclineTracker,
		},
		{
			name:       "parenthesized intake title",
			title:      "POS field-test findings (intake)",
			body:       "Collect findings before implementation issues are filed.",
			wantReason: admissionDeclineIntake,
		},
		{
			name:       "body self-identifies study artifact",
			title:      "POS parity",
			body:       "This issue is a study artifact for later implementation work.",
			wantReason: admissionDeclineStudy,
		},
		{
			name:       "body metadata identifies research artifact",
			title:      "POS parity",
			body:       "Type: research",
			wantReason: admissionDeclineResearch,
		},
		{
			name:       "linked issue checklist",
			title:      "Follow-up work",
			body:       "- [ ] #1\n- [x] owner/repo#2\n- [ ] https://github.com/owner/repo/issues/3",
			wantReason: admissionDeclineLinkedChecklist,
		},
		{
			name:       "operator marker overrides completion contract",
			title:      "Bounded work",
			body:       admissionOptOutMarker + "\n## Acceptance criteria\n\n- Complete the work.",
			wantReason: admissionDeclineExplicitOptOut,
		},
		{
			name:  "tracker with explicit completion contract fails open",
			title: "Dependency tracker",
			body:  "## Deliverable\n\nGenerate and publish one dependency inventory.",
		},
		{
			name:  "inline deliverable fails open",
			title: "Research (study)",
			body:  "Deliverable: publish one bounded comparison.",
		},
		{
			name:  "linked checklist under acceptance criteria fails open",
			title: "Bounded cleanup",
			body:  "## Acceptance criteria\n\n- [ ] Close #1\n- [ ] Close #2\n- [ ] Close #3",
		},
		{
			name:  "actionable research implementation",
			title: "Implement research ingestion",
			body:  "Add the endpoint and regression tests.",
		},
		{
			name:  "short linked checklist",
			title: "Follow-up",
			body:  "- [ ] #1\n- [ ] #2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, declined := classifyNonDeliverable(connector.Issue{Title: tt.title, Description: tt.body})
			if declined != (tt.wantReason != "") || got.reason != tt.wantReason {
				t.Fatalf("classifyNonDeliverable() = %#v, %t, want reason %q", got, declined, tt.wantReason)
			}
		})
	}
}

func TestProposalCommentQuotesCriteriaAndDoesNotUseStatusLabel(t *testing.T) {
	t.Parallel()

	proposal := admissionmodel.Proposal{
		ID:              "admission-1",
		TargetState:     "Todo",
		CriteriaSection: "Admission criteria",
		Findings: []admissionmodel.Finding{{
			Dimension:      "Alignment",
			CriterionQuote: "serves a stated current priority",
			Matched:        true,
			Rationale:      "Supports the release.",
		}},
		Confidence:        0.8,
		RecommendedEffort: "high",
		EffortRationale:   "The change crosses admission and dispatch.",
		ExpiresAt:         time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
	}
	criteria := config.AdmissionCriteria{Dimensions: []config.AdmissionDimension{{Name: "Alignment"}}}
	comment := proposalComment(proposal, criteria, false, false, "Backlog")
	for _, want := range []string{"serves a stated current priority", "have Detent move the issue", "admission-1", "Recommended effort: `high`", "crosses admission and dispatch"} {
		if !strings.Contains(comment, want) {
			t.Fatalf("proposalComment() missing %q: %s", want, comment)
		}
	}
	if strings.Contains(comment, "then move the issue") {
		t.Fatalf("proposalComment() still requires a manual move: %s", comment)
	}
	if strings.Contains(comment, "detent:admission") {
		t.Fatalf("proposalComment() introduced a status-prefixed label: %s", comment)
	}
	untrackedComment := proposalComment(proposal, criteria, false, true, "")
	for _, want := range []string{"no configured status label", "Acceptance is a two-part change", "assigning **Todo** status", "admitting the work for dispatch"} {
		if !strings.Contains(untrackedComment, want) {
			t.Fatalf("proposalComment(untracked) missing %q: %s", want, untrackedComment)
		}
	}
}
