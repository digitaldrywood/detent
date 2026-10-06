package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type NativeConnector struct {
	client *NativeClient
}

func NewNativeConnector(client *NativeClient) (*NativeConnector, error) {
	if client == nil {
		return nil, errors.New("native Hub client is required")
	}
	return &NativeConnector{client: client}, nil
}

func (c *NativeConnector) Name() string { return "hub_native" }

func (c *NativeConnector) NativeClient() *NativeClient { return c.client }

func (c *NativeConnector) Capabilities() connector.Capabilities {
	return connector.Capabilities{UpdateIssueState: true, SetAssignee: true, SetField: true, CreateComment: true, CreateWorkItems: true, UpdateComments: true}
}

func (c *NativeConnector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	project, err := c.client.Project(ctx)
	if err != nil {
		return nil, err
	}
	var states []string
	for _, state := range project.States {
		if state.Dispatchable && !state.Terminal {
			states = append(states, state.Name)
		}
	}
	return c.FetchIssuesByStates(ctx, states)
}

// WorkflowStates reports the native project's workflow, which is the state
// model the hub enforces on every transition.
func (c *NativeConnector) WorkflowStates(ctx context.Context) ([]connector.WorkflowState, error) {
	project, err := c.client.Project(ctx)
	if err != nil {
		return nil, err
	}
	states := make([]connector.WorkflowState, 0, len(project.States))
	for _, state := range project.States {
		states = append(states, connector.WorkflowState{
			Name: state.Name, Terminal: state.Terminal, Dispatchable: state.Dispatchable,
			OperatorOnly: state.OperatorOnly, Transitions: append([]string(nil), state.Transitions...),
		})
	}
	return states, nil
}

func (c *NativeConnector) FetchIssuesByStates(ctx context.Context, states []string) ([]connector.Issue, error) {
	var issues []connector.Issue
	for _, state := range states {
		cursor := ""
		for {
			page, err := c.client.Issues(ctx, url.Values{"state": {state}, "limit": {"2"}, "cursor": {cursor}})
			if err != nil {
				return nil, err
			}
			for _, issue := range page.Items {
				converted, err := c.issueWithLanding(ctx, issue)
				if err != nil {
					return nil, err
				}
				issues = append(issues, converted)
			}
			connector.ReportProgress(ctx)
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == cursor {
				return nil, errors.New("hub repeated issue cursor")
			}
			cursor = page.NextCursor
		}
	}
	return issues, nil
}

func (c *NativeConnector) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]connector.Issue, error) {
	issues := make([]connector.Issue, 0, len(ids))
	for _, id := range ids {
		issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				continue
			}
			return nil, err
		}
		converted, err := c.issueWithLanding(ctx, issue)
		if err != nil {
			return nil, err
		}
		issues = append(issues, converted)
	}
	return issues, nil
}

func (c *NativeConnector) issueWithLanding(ctx context.Context, native tracker.NativeIssue) (connector.Issue, error) {
	issue := issueFromNative(native)
	if strings.EqualFold(native.State, "Blocked") {
		attempts, history, err := c.recordedBlockerContext(ctx, native.WorkItemID)
		if err != nil {
			return connector.Issue{}, err
		}
		if attempt, prior, valid := tracker.RecordedNativeBlockers(native, attempts, history); valid {
			disposition := attempt.Disposition
			signal := &workpad.Signal{Source: workpad.SourceStructured, Status: disposition.Status, ReasonCode: disposition.ReasonCode, Blockers: disposition.BlockerEvidence, RecordedAt: &attempt.UpdatedAt}
			if disposition.HumanAction {
				signal.HumanAction = disposition.FinalSummary
				if signal.HumanAction == "" {
					signal.HumanAction = "Human action remains required"
				}
			}
			issue.WorkpadSignal = workpad.CloneSignal(signal)
			for index := range issue.WorkpadSignal.Blockers {
				blocker := &issue.WorkpadSignal.Blockers[index]
				if blocker.Predicate == nil || blocker.Predicate.Type != workpad.PredicateIssueState {
					continue
				}
				identifier, err := workpad.ParseRef(blocker.Predicate.Identifier, string(native.ProjectID))
				if err != nil {
					blocker.Unverifiable = true
					continue
				}
				blocker.Identifier, blocker.Predicate.Identifier = identifier, identifier
			}
			issue.BlockerReason = workpad.Reason(issue.WorkpadSignal)
			issue.Metadata["hub_disposition_attempt_id"] = attempt.AttemptID
			issue.Metadata["hub_disposition_return_state"] = prior
		}
	}
	if !native.Terminal {
		return issue, nil
	}
	changes, err := c.client.Changes(ctx, native.WorkItemID)
	if err != nil {
		return connector.Issue{}, err
	}
	for _, change := range changes {
		if change.WorkItemID != native.WorkItemID || change.Landed == nil {
			continue
		}
		if change.Landed.HeadSHA == "" || change.Landed.MergeSHA == "" {
			continue
		}
		issue.Metadata["hub_landed_head_sha"] = change.Landed.HeadSHA
		issue.Metadata["hub_landed_merge_sha"] = change.Landed.MergeSHA
	}
	return issue, nil
}

func (c *NativeConnector) recordedBlockerContext(ctx context.Context, id tracker.NativeWorkItemID) ([]tracker.NativeAttempt, []tracker.CollaborationEvent, error) {
	var attempts []tracker.NativeAttempt
	var history []tracker.CollaborationEvent
	for cursor := ""; ; {
		page, err := c.client.Attempts(ctx, id, cursor)
		if err != nil {
			return nil, nil, err
		}
		attempts = append(attempts, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for cursor := ""; ; {
		page, err := c.client.History(ctx, id, cursor)
		if err != nil {
			return nil, nil, err
		}
		history = append(history, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return attempts, history, nil
}

func (c *NativeConnector) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) ([]connector.Issue, error) {
	var issues []connector.Issue
	for _, identifier := range identifiers {
		if strings.HasPrefix(identifier, "wi_") {
			var native tracker.NativeIssue
			err := c.client.client.request(ctx, http.MethodGet, "/api/v2/organizations/"+string(c.client.organization)+"/work-items/"+url.PathEscape(identifier), nil, &native)
			if err != nil {
				return nil, err
			}
			client, err := c.client.client.Native(c.client.organization, native.ProjectID)
			if err != nil {
				return nil, err
			}
			converted, err := (&NativeConnector{client: client}).issueWithLanding(ctx, native)
			if err != nil {
				return nil, err
			}
			issues = append(issues, converted)
			continue
		}
		parsed, err := workpad.ParseRef(identifier, string(c.client.project))
		if err != nil {
			return nil, err
		}
		project, number, _ := strings.Cut(parsed, "#")
		if !strings.HasPrefix(project, "prj_") {
			return nil, errors.New("native dependency reference requires a native project")
		}
		client, err := c.client.client.Native(c.client.organization, tracker.ProjectID(project))
		if err != nil {
			return nil, err
		}
		for cursor := ""; ; {
			page, err := client.Issues(ctx, url.Values{"q": {parsed}, "limit": {"100"}, "cursor": {cursor}})
			if err != nil {
				return nil, err
			}
			for _, native := range page.Items {
				if strconv.Itoa(native.Number) == number && native.ProjectID == client.project {
					converted, err := (&NativeConnector{client: client}).issueWithLanding(ctx, native)
					if err != nil {
						return nil, err
					}
					issues = append(issues, converted)
				}
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	return issues, nil
}

func nativeMutationKey() tracker.Mutation { return tracker.Mutation{IdempotencyKey: uuid.NewString()} }

func nativeMutationKeyForContext(ctx context.Context) tracker.Mutation {
	return tracker.MutationForContext(ctx, nativeMutationKey().IdempotencyKey)
}

func (c *NativeConnector) CreateIssue(ctx context.Context, draft connector.IssueDraft) (connector.Issue, error) {
	return c.createIssue(ctx, intake.IssueDraft{Title: draft.Title, Body: draft.Body, Labels: draft.Labels}, "")
}

func (c *NativeConnector) createIssue(ctx context.Context, draft intake.IssueDraft, state string) (connector.Issue, error) {
	var priority *int
	if draft.Priority != nil {
		if *draft.Priority < 1 || *draft.Priority > 4 {
			return connector.Issue{}, errors.New("priority must be an integer creation rank between 1 and 4")
		}
		level := *draft.Priority - 1
		priority = &level
	}
	authority, bound := ctx.Value(nativeMutationAuthorityKey{}).(nativeMutationAuthority)
	hostIntake := state == "Backlog" && bound && authority.scope == c.client.base()
	if origin, machine := issueorigin.Parse(draft.Body); machine && !hostIntake {
		existing, found, err := c.findIntakeIssue(ctx, func(issue tracker.NativeIssue) bool {
			previous, ok := issueorigin.Parse(issue.Body)
			return !issue.Terminal && ok && previous.Fingerprint == origin.Fingerprint
		})
		if err != nil {
			return connector.Issue{}, err
		}
		if found {
			if err := c.CreateComment(ctx, existing.ID, issueorigin.Occurrence(draft.Body)); err != nil {
				return connector.Issue{}, err
			}
			issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(existing.ID))
			result := issueFromNative(issue)
			result.PublicationReused = true
			return result, err
		}
	}
	project, err := c.client.Project(ctx)
	if err != nil {
		return connector.Issue{}, err
	}
	if len(project.States) == 0 {
		return connector.Issue{}, errors.New("native project has no workflow states")
	}
	if state == "" {
		state = project.States[0].Name
	}
	mutation := nativeMutationKeyForContext(ctx)
	if hostIntake {
		mutation.LeaseID = authority.lease.ID
		mutation.FencingToken = authority.lease.FencingToken
	}
	issue, err := c.client.CreateIssue(ctx, tracker.CreateIssue{Mutation: mutation, Title: draft.Title, Body: draft.Body, Labels: draft.Labels, State: state, Priority: priority})
	result := issueFromNative(issue)
	result.PublicationReused = issue.PublicationReused
	return result, err
}

func (c *NativeConnector) FindIntakeIssue(ctx context.Context, marker string) (intake.Issue, bool, error) {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return intake.Issue{}, false, nil
	}
	return c.findIntakeIssue(ctx, func(issue tracker.NativeIssue) bool { return strings.Contains(issue.Body, marker) })
}

func (c *NativeConnector) findIntakeIssue(ctx context.Context, match func(tracker.NativeIssue) bool) (intake.Issue, bool, error) {
	var closed intake.Issue
	cursor := ""
	for {
		page, err := c.client.Issues(ctx, url.Values{"limit": {"20"}, "cursor": {cursor}})
		if err != nil {
			return intake.Issue{}, false, err
		}
		for _, issue := range page.Items {
			if issue.Archived || !match(issue) {
				continue
			}
			result := nativeIntakeIssue(issueFromNative(issue))
			if !result.Closed {
				return result, true, nil
			}
			if result.Number > closed.Number {
				closed = result
			}
		}
		connector.ReportProgress(ctx)
		if page.NextCursor == "" {
			return closed, closed.ID != "", nil
		}
		if page.NextCursor == cursor {
			return intake.Issue{}, false, errors.New("hub repeated issue cursor")
		}
		cursor = page.NextCursor
	}
}

func (c *NativeConnector) CreateIntakeIssue(ctx context.Context, draft intake.IssueDraft) (intake.Issue, error) {
	issue, err := c.createIssue(ctx, draft, "Backlog")
	return nativeIntakeIssue(issue), err
}

func (c *NativeConnector) UpdateIntakeIssue(ctx context.Context, id string, draft intake.IssueDraft) (intake.Issue, error) {
	issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
	if err != nil {
		return intake.Issue{}, err
	}
	labels := slices.Clone(issue.Labels)
	for _, label := range draft.Labels {
		if !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	body := issueorigin.Preserve(draft.Body, issue.Body)
	updated, err := c.client.UpdateIssue(ctx, issue.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKeyForContext(ctx), ExpectedRevision: issue.Revision, Title: &draft.Title, Body: &body, Labels: &labels})
	return nativeIntakeIssue(issueFromNative(updated)), err
}

func (c *NativeConnector) SetIntakeIssueState(ctx context.Context, id, state string) error {
	return c.UpdateIssueState(ctx, id, state)
}

func nativeIntakeIssue(issue connector.Issue) intake.Issue {
	return intake.Issue{State: issue.State, ID: issue.ID, Identifier: issue.Identifier, Number: issue.Number, URL: issue.URL, Body: issue.Description, Closed: issue.Closed, Reused: issue.PublicationReused}
}

// ChangeReviewed reports whether the given version is the change's current
// version and is reviewed: accepted by the project's review policy, with
// every approval and check it asks for in place. Any other current version
// is not this run's, so it never answers for it.
func (c *NativeConnector) ChangeReviewed(ctx context.Context, issueID, changeID, versionID string) (bool, error) {
	detail, err := c.client.Change(ctx, tracker.NativeWorkItemID(issueID), changeID)
	if err != nil {
		return false, err
	}
	return versionID != "" && detail.Change.CurrentVersion == versionID && detail.Summary.Status == "reviewed", nil
}

func (c *NativeConnector) CreateComment(ctx context.Context, id, body string) error {
	_, err := c.client.CreateComment(ctx, tracker.NativeWorkItemID(id), tracker.CreateComment{Mutation: nativeMutationKeyForContext(ctx), Body: body})
	return err
}

func (c *NativeConnector) UpdateIssueState(ctx context.Context, id, state string) error {
	issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	expected := tracker.ExpectedRevision(ctx)
	if expected > 0 && expected != issue.Revision {
		return errors.New("native issue revision is stale")
	}
	if expected <= 0 {
		expected = issue.Revision
	}
	if issue.State == state {
		return nil
	}
	request := tracker.Transition{Mutation: nativeMutationKeyForContext(ctx), ExpectedRevision: expected, State: state, Reason: "worker_progress", ReasonDetail: connector.LaneTransitionReason(ctx)}
	if strings.ReplaceAll(request.ReasonDetail, " ", "_") == "recorded_blocker_recovery" {
		attempts, history, readErr := c.recordedBlockerContext(ctx, issue.WorkItemID)
		if readErr != nil {
			return readErr
		}
		if attempt, prior, valid := tracker.RecordedNativeBlockers(issue, attempts, history); valid && prior == state {
			approval, err := c.client.ProjectPolicy(ctx)
			if err != nil {
				return err
			}
			request.Reason = "dependency_ready"
			request.PolicyID = approval.Policy.ID
			request.BlockerAttemptID = attempt.AttemptID
			request.LeaseID, request.FencingToken = attempt.LeaseID, attempt.FencingToken
		}
	}
	_, err = c.client.Transition(ctx, issue.WorkItemID, request)
	return err
}

func (c *NativeConnector) SetAssignee(ctx context.Context, id, login string) error {
	issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	assignees := []string{}
	if login != "" {
		assignees = append(assignees, login)
	}
	_, err = c.client.UpdateIssue(ctx, issue.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKeyForContext(ctx), ExpectedRevision: issue.Revision, Assignees: &assignees})
	return err
}

func (c *NativeConnector) SetField(ctx context.Context, id, field, value string) error {
	if field == "state" {
		return c.UpdateIssueState(ctx, id, value)
	}
	if field == "assignee" {
		return c.SetAssignee(ctx, id, value)
	}
	issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	request := tracker.UpdateIssue{Mutation: nativeMutationKeyForContext(ctx), ExpectedRevision: issue.Revision}
	switch field {
	case "title":
		request.Title = &value
	case "body":
		request.Body = &value
	case "priority":
		priority, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		request.Priority = tracker.SetPriority(&priority)
	default:
		return connector.ErrNotImplemented
	}
	_, err = c.client.UpdateIssue(ctx, issue.WorkItemID, request)
	return err
}

func (c *NativeConnector) UpdateIssueBody(ctx context.Context, id, body string) error {
	return c.SetField(ctx, id, "body", body)
}

func (c *NativeConnector) FetchIssueComments(ctx context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	var comments []connector.IssueComment
	cursor := ""
	for {
		page, err := c.client.Comments(ctx, tracker.NativeWorkItemID(issue.ID), cursor)
		if err != nil {
			return nil, err
		}
		for _, comment := range page.Items {
			mapped := connector.IssueComment{ID: comment.ID, Backend: c.Name(), Body: comment.Body, AuthorLogin: comment.Actor.PrincipalID, AuthorKind: comment.Actor.Kind,
				CreatedAt: cloneTime(&comment.CreatedAt), UpdatedAt: cloneTime(&comment.UpdatedAt), CanEdit: true, TargetType: "issue", AuthorAuthorized: comment.Actor.Kind == "human" && comment.Provenance == nil}
			if comment.Provenance != nil {
				mapped.AuthorLogin = comment.Provenance.AuthorID
				mapped.AuthorDisplayName = comment.Provenance.AuthorDisplayName
				mapped.AuthorKind = "imported"
			}
			comments = append(comments, mapped)
		}
		connector.ReportProgress(ctx)
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, errors.New("hub repeated comment cursor")
		}
		cursor = page.NextCursor
	}
	return comments, nil
}

func (c *NativeConnector) IsIssueCommentAuthorAuthorized(_ context.Context, _ connector.Issue, comment connector.IssueComment) (bool, error) {
	return comment.Backend == c.Name() && comment.AuthorKind == "human" && comment.AuthorAuthorized, nil
}

func (c *NativeConnector) UpdateIssueComment(ctx context.Context, id, commentID, body string) error {
	cursor := ""
	for {
		page, err := c.client.Comments(ctx, tracker.NativeWorkItemID(id), cursor)
		if err != nil {
			return err
		}
		for _, comment := range page.Items {
			if comment.ID == commentID {
				_, err := c.client.UpdateComment(ctx, tracker.NativeWorkItemID(id), commentID, tracker.UpdateComment{Mutation: nativeMutationKeyForContext(ctx), ExpectedRevision: comment.Revision, Body: body})
				return err
			}
		}
		if page.NextCursor == "" {
			return errors.New("native comment was not found")
		}
		if page.NextCursor == cursor {
			return errors.New("hub repeated comment cursor")
		}
		cursor = page.NextCursor
	}
}

func (c *NativeConnector) FetchIssueEvents(ctx context.Context, issue connector.Issue) ([]connector.IssueEvent, error) {
	var events []connector.IssueEvent
	cursor := ""
	for {
		page, err := c.client.History(ctx, tracker.NativeWorkItemID(issue.ID), cursor)
		if err != nil {
			return nil, err
		}
		for _, event := range page.Items {
			events = append(events, connector.IssueEvent{ID: event.ID, Kind: event.Type, State: event.Data.ToState, CreatedAt: cloneTime(&event.RecordedAt), Actor: connector.IssueActor{Login: event.Actor.PrincipalID}})
		}
		connector.ReportProgress(ctx)
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, errors.New("hub repeated history cursor")
		}
		cursor = page.NextCursor
	}
	return events, nil
}

func (c *NativeConnector) AddIssueDependency(ctx context.Context, issueID, blockerID string) error {
	return c.dependency(ctx, issueID, blockerID, "add")
}

func (c *NativeConnector) RemoveIssueDependency(ctx context.Context, issueID, blockerID string) error {
	return c.dependency(ctx, issueID, blockerID, "remove")
}

func (c *NativeConnector) dependency(ctx context.Context, id, blockerID, operation string) error {
	issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	_, err = c.client.Dependency(ctx, issue.WorkItemID, tracker.DependencyMutation{Mutation: nativeMutationKeyForContext(ctx), ExpectedRevision: issue.Revision, RelatedWorkItemID: tracker.NativeWorkItemID(blockerID), Operation: operation})
	return err
}

// nativeIssueIdentifier is the identifier a run of a native issue keys its
// worktree on.
func nativeIssueIdentifier(native tracker.NativeIssue) string {
	return string(native.ProjectID) + "#" + strconv.Itoa(native.Number)
}

func issueFromNative(native tracker.NativeIssue) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = string(native.WorkItemID)
	issue.Identifier = nativeIssueIdentifier(native)
	issue.Number = native.Number
	issue.Title, issue.Description, issue.State = native.Title, native.Body, native.State
	issue.DependencySource = connector.BlockedRefSourceNative
	issue.AuthorID = native.Actor.PrincipalID
	issue.Closed = native.Terminal
	issue.Labels, issue.Assignees = native.Labels, native.Assignees
	issue.CreatedAt, issue.UpdatedAt = cloneTime(&native.CreatedAt), cloneTime(&native.UpdatedAt)
	issue.Metadata["hub_organization_id"] = string(native.OrganizationID)
	issue.Metadata["hub_project_id"] = string(native.ProjectID)
	issue.Metadata["hub_revision"] = strconv.FormatInt(int64(native.Revision), 10)
	issue.Metadata["hub_profile"] = "native"
	if native.Priority != nil {
		priority := *native.Priority + 1
		issue.Priority = &priority
		issue.PriorityName = queuePriorityName(*native.Priority)
	}
	for _, blocker := range native.Blockers {
		if native.IgnoreDependencies {
			break
		}
		state := "open"
		if blocker.Terminal {
			state = "closed"
		}
		identifier := blocker.Identifier
		if identifier == "" {
			identifier = string(blocker.ID)
		}
		issue.BlockedBy = append(issue.BlockedBy, connector.BlockedRef{ID: string(blocker.ID), Identifier: identifier, State: blocker.State, TrackerState: state, Source: connector.BlockedRefSourceNative})
	}
	if native.Provenance != nil {
		issue.AuthorID = strings.TrimSpace(native.Provenance.AuthorID)
	}
	return issue
}

func (c *NativeConnector) DispatchProjectRank() int {
	r := c.client.client.runner
	if r == nil {
		return 2147483647
	}
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	if r.routing == nil {
		return 2147483647
	}
	if rank, ok := r.routing.Routing.ProjectRanks[c.client.project]; ok {
		return rank
	}
	return 2147483647
}
