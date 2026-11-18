package boardview

import (
	"cmp"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/boardoverlay"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemread"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Cursor struct {
	State      string
	More       bool
	ClosedAt   *time.Time
	Identifier string
}

type boardViewLink struct {
	relation string
	ordinal  int64
	value    tracker.NativeDependency
}

func boardViewText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func boardViewNumber(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func boardViewFlag(value *bool) bool { return value != nil && *value }

func boardViewStamp(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

type Project struct {
	Project                   tracker.NativeProject      `json:"project"`
	Sequence                  int64                      `json:"sequence"`
	ProjectionPending         bool                       `json:"projection_pending"`
	ProjectionOldestPendingAt *time.Time                 `json:"projection_oldest_pending_at,omitempty"`
	Work                      *tracker.NativeWorkSummary `json:"work,omitempty"`
}

type View struct {
	Projects []Project
	Cards    []tracker.NativeBoardCard
	Running  map[tracker.NativeWorkItemID]bool
}

func Assemble(scope hubscope.NativeScope, items bool, rows viewstore.BoardViewRows, window string, now time.Time, closedPage int) (View, []Cursor, error) {
	view := View{Projects: []Project{}, Cards: []tracker.NativeBoardCard{}, Running: map[tracker.NativeWorkItemID]bool{}}
	type stateRow struct {
		rank, transitionRank int64
		state                tracker.NativeState
		target               *string
	}
	projects := map[tracker.ProjectID]*Project{}
	var order []tracker.ProjectID
	states := map[tracker.ProjectID][]stateRow{}
	summaries := map[tracker.ProjectID][]viewstore.BoardSummaryRecord{}
	completions := map[tracker.ProjectID][]viewstore.BoardSummaryRecord{}
	links := map[string][]boardViewLink{}
	dependencies := map[string][]boardViewLink{}
	reasons := map[string]viewstore.BoardViewRow{}
	changes := map[string]bool{}
	facts := map[tracker.ProjectID]*policystore.ProjectOverlayFacts{}
	checks := map[tracker.ProjectID]map[string][]viewstore.BoardViewRow{}
	factsOf := func(project tracker.ProjectID) *policystore.ProjectOverlayFacts {
		if facts[project] == nil {
			facts[project] = &policystore.ProjectOverlayFacts{Policies: map[string]policystore.Compatibility{}, Reviews: map[string]policystore.ReviewCompatibility{}}
		}
		return facts[project]
	}
	sequences := map[tracker.ProjectID]viewstore.BoardViewRow{}
	var cards, closed []viewstore.BoardViewRow
	for _, row := range rows.Rows {
		project := tracker.ProjectID(boardViewText(row.Project))
		switch row.Kind {
		case "project":
			projects[project] = &Project{Project: tracker.NativeProject{OrganizationID: scope.Organization, ID: project, Name: boardViewText(row.Text[0]), Slug: boardViewText(row.Text[1]), Profile: boardViewText(row.Text[2]), RequireDependencies: boardViewFlag(row.Bool[0]), States: []tracker.NativeState{}}}
			order = append(order, project)
		case "state":
			states[project] = append(states[project], stateRow{rank: boardViewNumber(row.Int[0]), transitionRank: boardViewNumber(row.Int[1]), target: row.Text[0],
				state: tracker.NativeState{Name: boardViewText(row.ID), Terminal: boardViewFlag(row.Bool[0]), Dispatchable: boardViewFlag(row.Bool[1]), OperatorOnly: boardViewFlag(row.Bool[2]), Transitions: []string{}}})
		case "sequence":
			sequences[project] = row
		case "count":
			if boardViewText(row.Text[1]) == "all" {
				completions[project] = append(completions[project], viewstore.BoardSummaryRecord{Kind: boardViewText(row.Text[0]), State: boardViewText(row.ID),
					Shipped1h: boardViewNumber(row.Int[2]), Shipped24h: boardViewNumber(row.Int[3]), Shipped7d: boardViewNumber(row.Int[4]), Cancelled1h: boardViewNumber(row.Int[5]), Cancelled24h: boardViewNumber(row.Int[6]), Cancelled7d: boardViewNumber(row.Int[7]), ProjectedAt: boardViewStamp(row.Time[0])})
				continue
			}
			summaries[project] = append(summaries[project], viewstore.BoardSummaryRecord{Kind: boardViewText(row.Text[0]), State: boardViewText(row.ID), Total: boardViewNumber(row.Int[0]), Running: boardViewNumber(row.Int[1]),
				Shipped1h: boardViewNumber(row.Int[2]), Shipped24h: boardViewNumber(row.Int[3]), Shipped7d: boardViewNumber(row.Int[4]), Cancelled1h: boardViewNumber(row.Int[5]), Cancelled24h: boardViewNumber(row.Int[6]), Cancelled7d: boardViewNumber(row.Int[7]), ProjectedAt: boardViewStamp(row.Time[0])})
		case "card":
			cards = append(cards, row)
		case "closed":
			closed = append(closed, row)
		case "link":
			if boardViewFlag(row.Bool[2]) {
				links[boardViewText(row.ID)] = append(links[boardViewText(row.ID)], boardViewLink{relation: boardViewText(row.Text[0]), ordinal: boardViewNumber(row.Int[1]), value: tracker.NativeDependency{
					ID: tracker.NativeWorkItemID(boardViewText(row.Text[1])), ProjectID: project, Title: boardViewText(row.Text[2]), State: boardViewText(row.Text[3]), Identifier: boardViewText(row.Text[4]), WebURL: boardViewText(row.Text[5]),
					Number: int(boardViewNumber(row.Int[0])), Terminal: boardViewFlag(row.Bool[0]), Dispatchable: boardViewFlag(row.Bool[1]), ClosedAt: row.Time[0], StartedAt: row.Time[1]}})
			}
		case "dependency":
			if boardViewFlag(row.Bool[2]) {
				dependencies[boardViewText(row.ID)] = append(dependencies[boardViewText(row.ID)], boardViewLink{ordinal: boardViewNumber(row.Int[1]), value: tracker.NativeDependency{ID: tracker.NativeWorkItemID(boardViewText(row.Text[1]))}})
			}
		case "reason":
			reasons[boardViewText(row.ID)] = row
		case "change":
			changes[string(project)+"\x00"+boardViewText(row.ID)] = true
		case "policy":
			f := factsOf(project)
			f.PolicyID, f.ApprovedBy, f.ApprovedAt = boardViewText(row.ID), boardViewText(row.Text[0]), boardViewStamp(row.Time[0])
		case "review":
			f := factsOf(project)
			f.ReviewPolicyID, f.ReviewPolicy, f.ReviewBy, f.ReviewRevision, f.ReviewAt = boardViewText(row.ID), boardViewText(row.Text[0]), boardViewText(row.Text[1]), boardViewNumber(row.Int[0]), boardViewStamp(row.Time[0])
		case "policy_facts":
			factsOf(project).Policies[boardViewText(row.ID)] = policystore.Compatibility{PayloadSHA256: boardViewText(row.Text[0]), Gates: changerequest.CompatibilityGates{
				LandingMode: boardViewText(row.Text[1]), Kind: boardViewText(row.Text[2]), LandingCommandDigest: boardViewText(row.Text[3]), GitHubCIDigest: boardViewText(row.Text[4]), PlanReview: boardViewText(row.Text[5]),
				AutomatedReview: boardViewText(row.Text[6]), RequiredChecks: int(boardViewNumber(row.Int[0])), GitHubPullRequest: boardViewFlag(row.Bool[0]), Validator: boardViewFlag(row.Bool[1]),
				SecurityAudit: boardViewFlag(row.Bool[2]), HumanReview: boardViewFlag(row.Bool[3]), PlanEnabled: boardViewFlag(row.Bool[4])}}
		case "review_facts":
			factsOf(project).Reviews[boardViewText(row.ID)] = policystore.ReviewCompatibility{PolicyID: boardViewText(row.Text[0]), PayloadSHA256: boardViewText(row.Text[1]),
				ReviewFacts: policystore.ReviewFacts{RequireReview: boardViewFlag(row.Bool[0]), RequiredChecks: make([]tracker.ChangeCheckSpec, 0, boardViewNumber(row.Int[0]))}}
		case "review_check":
			if checks[project] == nil {
				checks[project] = map[string][]viewstore.BoardViewRow{}
			}
			checks[project][boardViewText(row.ID)] = append(checks[project][boardViewText(row.ID)], row)
		}
	}
	for project, list := range states {
		entry := projects[project]
		if entry == nil {
			continue
		}
		slices.SortStableFunc(list, func(a, b stateRow) int {
			return cmp.Or(cmp.Compare(a.rank, b.rank), cmp.Compare(a.state.Name, b.state.Name), cmp.Compare(a.transitionRank, b.transitionRank))
		})
		for _, row := range list {
			if n := len(entry.Project.States); n == 0 || entry.Project.States[n-1].Name != row.state.Name {
				entry.Project.States = append(entry.Project.States, row.state)
			}
			if row.target != nil {
				last := &entry.Project.States[len(entry.Project.States)-1]
				last.Transitions = append(last.Transitions, *row.target)
			}
		}
	}
	overlays := map[tracker.ProjectID]boardoverlay.Overlay{}
	for project, f := range facts {
		for id, review := range f.Reviews {
			listed := checks[project][id]
			slices.SortFunc(listed, func(a, b viewstore.BoardViewRow) int {
				return cmp.Compare(boardViewNumber(a.Int[0]), boardViewNumber(b.Int[0]))
			})
			if len(listed) != cap(review.RequiredChecks) {
				return view, nil, viewstore.ErrInvalidView
			}
			for i, check := range listed {
				if boardViewNumber(check.Int[0]) != int64(i) {
					return view, nil, viewstore.ErrInvalidView
				}
				review.RequiredChecks = append(review.RequiredChecks, tracker.ChangeCheckSpec{Name: boardViewText(check.Text[0]), PrincipalID: boardViewText(check.Text[1]), WorkflowID: boardViewText(check.Text[2]), WorkflowSHA256: boardViewText(check.Text[3]), Source: boardViewText(check.Text[4]), MaxAgeSeconds: boardViewNumber(check.Int[1])})
			}
			f.Reviews[id] = review
		}
		overlay, err := boardoverlay.FromFacts(scope.Organization, project, *f)
		if err != nil {
			return view, nil, err
		}
		overlays[project] = overlay
	}
	for _, project := range order {
		entry := projects[project]
		if row, found := sequences[project]; found {
			entry.Sequence, entry.ProjectionPending, entry.ProjectionOldestPendingAt = boardViewNumber(row.Int[0]), boardViewFlag(row.Bool[0]), row.Time[0]
		}
		if !entry.ProjectionPending {
			entry.Work = work(summaries[project], completions[project], window, now)
		}
		view.Projects = append(view.Projects, *entry)
	}
	build := func(row viewstore.BoardViewRow) tracker.NativeBoardCard {
		c := row.Card
		id := boardViewText(row.ID)
		project := tracker.ProjectID(boardViewText(row.Project))
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.Organization, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(id), Number: int(boardViewNumber(c.Number)), Revision: tracker.Revision(boardViewNumber(c.Revision)), Profile: "native"},
			Type: boardViewText(c.IssueType), Title: boardViewText(c.Title), State: boardViewText(c.State), Archived: boardViewFlag(c.Archived), Terminal: boardViewFlag(c.Terminal), Labels: c.Labels, Assignees: c.Assignees,
			Actor:  tracker.Actor{Kind: boardViewText(c.ActorKind), PrincipalID: boardViewText(c.ActorPrincipalID), ViaKey: boardViewText(c.ActorViaKey), DisplayName: cmp.Or(boardViewText(c.ActorPublicName), boardViewText(c.ActorDisplayName)), Agent: boardViewText(c.ActorAgent)},
			WebURL: boardViewText(c.WebURL), OmittedFields: c.OmittedFields, ReporterStatus: boardViewText(c.ReporterStatus), SchedulingEligible: c.SchedulingEligible,
			CreatedAt: boardViewStamp(c.CreatedAt), UpdatedAt: boardViewStamp(c.UpdatedAt), ClosedAt: c.ClosedAt, LastActivityAt: boardViewStamp(c.LastActivityAt), DependencyReady: true}
		if scope.Credential.HostedRole == "reporter" {
			issue.ReporterStatus = boardViewText(c.ReporterViewStatus)
		}
		if c.Priority != nil {
			issue.Priority = new(int(*c.Priority))
		}
		if boardViewText(c.ProvenanceProvider) != "" {
			issue.Provenance = &tracker.Provenance{Provider: boardViewText(c.ProvenanceProvider), ExternalID: boardViewText(c.ProvenanceExternalID), AuthorID: boardViewText(c.ProvenanceAuthorID), AuthorDisplayName: boardViewText(c.ProvenanceAuthorDisplayName),
				CreatedAt: boardViewStamp(c.ProvenanceCreatedAt), UpdatedAt: boardViewStamp(c.ProvenanceUpdatedAt), ObservedAt: boardViewStamp(c.ProvenanceObservedAt)}
		}
		if boardViewFlag(c.VerificationPresent) {
			issue.Verification = &tracker.NativeVerification{State: boardViewText(c.VerificationState), VerifiedAt: c.VerifiedAt}
			if boardViewFlag(c.VerifiedByPresent) {
				issue.Verification.VerifiedBy = &tracker.Actor{Kind: boardViewText(c.VerifiedByKind), PrincipalID: boardViewText(c.VerifiedByPrincipalID), ViaKey: boardViewText(c.VerifiedByViaKey), DisplayName: boardViewText(c.VerifiedByDisplayName), Agent: boardViewText(c.VerifiedByAgent)}
			}
		}
		if boardViewFlag(c.ReportContextPresent) {
			issue.ReportContext = &tracker.ReportContext{URL: boardViewText(c.ReportContextURL), AppBuild: boardViewText(c.ReportContextAppBuild), Browser: boardViewText(c.ReportContextBrowser), OS: boardViewText(c.ReportContextOS), Viewport: boardViewText(c.ReportContextViewport), CapturedAt: boardViewText(c.ReportContextCapturedAt)}
		}
		if boardViewFlag(c.ContractPresent) {
			issue.IssueContract = &issuecontract.State{HumanAction: boardViewText(c.ContractHumanAction)}
		}
		if c.PullRequestNumber != nil {
			issue.PullRequest = &tracker.NativePullRequest{Number: int(*c.PullRequestNumber), URL: boardViewText(c.PullRequestURL)}
		}
		if c.ListChangeBranch != nil {
			issue.ListChange = &tracker.NativeListChange{Branch: *c.ListChangeBranch, HeadSHA: boardViewText(c.ListChangeHeadSHA)}
		}
		related := links[id]
		slices.SortStableFunc(related, func(a, b boardViewLink) int { return cmp.Compare(a.ordinal, b.ordinal) })
		for _, link := range related {
			switch link.relation {
			case "blocker":
				issue.Blockers = append(issue.Blockers, link.value)
			case "dependent":
				issue.Dependents = append(issue.Dependents, link.value)
			case "related":
				issue.Related = append(issue.Related, link.value)
			}
		}
		listed := dependencies[id]
		slices.SortStableFunc(listed, func(a, b boardViewLink) int { return cmp.Compare(a.ordinal, b.ordinal) })
		for _, link := range listed {
			issue.Dependencies = append(issue.Dependencies, link.value.ID)
		}
		issue.DependencyReady = !slices.ContainsFunc(issue.Blockers, func(link tracker.NativeDependency) bool { return !link.Terminal })
		blocked, unblocks := make([]string, 0, len(issue.Dependencies)), make([]string, 0, len(issue.Dependents))
		for _, dependency := range issue.Dependencies {
			blocked = append(blocked, string(dependency))
		}
		for _, link := range issue.Dependents {
			unblocks = append(unblocks, string(link.ID))
		}
		cross := func(link tracker.NativeDependency) bool { return link.ProjectID != project }
		reason := reasons[id]
		issue.BlockedReason = workitemread.VisibleDependencyReason(tracker.DependencyReason{Text: boardViewText(reason.Text[0]), Basis: boardViewText(reason.Text[1]), CurrentBasis: boardViewText(reason.Text[2]), Revision: boardViewNumber(reason.Int[0])}, blocked, slices.ContainsFunc(issue.Blockers, cross))
		issue.UnblocksReason = workitemread.VisibleDependencyReason(tracker.DependencyReason{Text: boardViewText(reason.Text[3]), Basis: boardViewText(reason.Text[4]), CurrentBasis: boardViewText(reason.Text[5]), Revision: boardViewNumber(reason.Int[1])}, unblocks, slices.ContainsFunc(issue.Dependents, cross))
		card := tracker.NativeBoardCard{Issue: issue}
		if boardViewFlag(c.Running) {
			view.Running[issue.WorkItemID] = true
		}
		if boardViewText(c.AttemptID) != "" {
			card.Attempt = &tracker.NativeBoardAttempt{ID: boardViewText(c.AttemptID), Count: int(boardViewNumber(c.AttemptCount)), Status: boardViewText(c.AttemptStatus), RunnerID: boardViewText(c.AttemptRunnerID), MachineID: boardViewText(c.AttemptMachineID), StartedAt: boardViewStamp(c.AttemptStartedAt), ExpiresAt: boardViewStamp(c.AttemptExpiresAt)}
			if identity := (tracker.NativeExecutionIdentity{Role: boardViewText(c.AttemptRole), Backend: boardViewText(c.AttemptBackend), Model: boardViewText(c.AttemptModel), ReasoningEffort: boardViewText(c.AttemptEffort)}); identity != (tracker.NativeExecutionIdentity{}) {
				card.Attempt.Identity = &identity
			}
		}
		if boardViewText(c.ExecutionID) != "" {
			card.Execution = &tracker.NativeBoardExecution{ID: boardViewText(c.ExecutionID), Running: boardViewFlag(c.ExecutionRunning), Stage: boardViewText(c.ExecutionStage), RunnerID: boardViewText(c.ExecutionRunnerID), StartedAt: c.ExecutionStartedAt, ExpiresAt: boardViewStamp(c.ExecutionExpiresAt)}
			if identity := (tracker.NativeExecutionIdentity{Role: boardViewText(c.ExecutionRole), Backend: boardViewText(c.ExecutionBackend), Model: boardViewText(c.ExecutionModel), ReasoningEffort: boardViewText(c.ExecutionEffort)}); identity != (tracker.NativeExecutionIdentity{}) {
				card.Execution.Identity = &identity
			}
		}
		owned := scope.ReporterOwns(tracker.Actor{PrincipalID: boardViewText(c.ChangePrincipalID)})
		visible := boardViewText(c.ChangeID) != "" && changes[string(project)+"\x00"+boardViewText(c.ChangeID)] && owned
		if !owned || boardViewText(c.ChangeID) != "" && !visible {
			card.Issue.ListChange, card.Issue.PullRequest = nil, nil
		}
		if visible {
			card.Change = &tracker.NativeBoardChange{ID: boardViewText(c.ChangeID), Title: boardViewText(c.ChangeTitle), Status: boardViewText(c.ChangeStatus)}
			if boardViewText(c.ChangeProvider) != "" {
				card.Change.External = &tracker.ChangeExternalReference{Provider: boardViewText(c.ChangeProvider), ID: boardViewText(c.ChangeExternalID), URL: boardViewText(c.ChangeExternalURL)}
			}
			record := viewstore.BoardRecord{Display: viewstore.BoardDisplay{Change: card.Change, ChangeProvenance: &viewstore.BoardChangeProvenance{VersionID: boardViewText(c.ChangeVersionID), PolicyID: boardViewText(c.ChangePolicyID), ReviewPolicyID: boardViewText(c.ChangeReviewPolicyID), BaseStatus: boardViewText(c.ChangeBaseStatus)}}}
			card.Change.Status = overlays[project].Status(record)
		}
		return card
	}
	for _, row := range cards {
		view.Cards = append(view.Cards, build(row))
	}
	if items {
		return view, nil, nil
	}
	byLane := map[string][]viewstore.BoardViewRow{}
	var lanes []string
	for _, row := range closed {
		state := boardViewText(row.Card.State)
		if _, found := byLane[state]; !found {
			lanes = append(lanes, state)
		}
		byLane[state] = append(byLane[state], row)
	}
	slices.Sort(lanes)
	var positions []Cursor
	for _, lane := range lanes {
		list := byLane[lane]
		slices.SortFunc(list, func(a, b viewstore.BoardViewRow) int {
			return cmp.Or(cmp.Compare(boardViewStamp(b.Card.ClosedAt).UnixNano(), boardViewStamp(a.Card.ClosedAt).UnixNano()), cmp.Compare(boardViewText(b.Card.Identifier), boardViewText(a.Card.Identifier)))
		})
		position := Cursor{State: lane}
		if len(list) > closedPage {
			list = list[:closedPage]
			last := list[len(list)-1]
			position.More, position.ClosedAt, position.Identifier = true, last.Card.ClosedAt, boardViewText(last.Card.Identifier)
		}
		for _, row := range list {
			view.Cards = append(view.Cards, build(row))
		}
		positions = append(positions, position)
	}
	return view, positions, nil
}

func work(summaries, completions []viewstore.BoardSummaryRecord, selected string, now time.Time) *tracker.NativeWorkSummary {
	summary := &tracker.NativeWorkSummary{Items: []tracker.NativeIssue{}, Lanes: []tracker.NativeWorkLane{}, ShippedWindows: []tracker.NativeShippedWindow{}, AsOf: now}
	windows := []tracker.NativeShippedWindow{{Window: "1h"}, {Window: "24h"}, {Window: "7d"}}
	shipped := func(source viewstore.BoardSummaryRecord) {
		windows[0].Shipped, windows[0].Cancelled = int(source.Shipped1h), int(source.Cancelled1h)
		windows[1].Shipped, windows[1].Cancelled = int(source.Shipped24h), int(source.Cancelled24h)
		windows[2].Shipped, windows[2].Cancelled = int(source.Shipped7d), int(source.Cancelled7d)
	}
	var completion *viewstore.BoardSummaryRecord
	for i := range completions {
		if completions[i].Kind == "total" {
			completion = &completions[i]
		}
	}
	for _, row := range summaries {
		switch row.Kind {
		case "lane":
			summary.Lanes = append(summary.Lanes, tracker.NativeWorkLane{State: row.State, Total: int(row.Total), Running: int(row.Running)})
		case "total":
			shipped(row)
			summary.AsOf = row.ProjectedAt
		}
	}
	if completion != nil {
		shipped(*completion)
		if completion.ProjectedAt.After(summary.AsOf) || summary.AsOf.Equal(now) {
			summary.AsOf = completion.ProjectedAt
		}
	}
	slices.SortFunc(summary.Lanes, func(a, b tracker.NativeWorkLane) int { return cmp.Compare(a.State, b.State) })
	summary.ShippedWindows = windows
	for _, window := range windows {
		if window.Window == selected {
			summary.Completed, summary.Cancelled = window.Shipped, window.Cancelled
		}
	}
	return summary
}
