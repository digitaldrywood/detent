package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type dashboardWorkReads struct{ server *Server }

func (r dashboardWorkReads) WorkReadNames(ctx context.Context) []string {
	s := r.server
	if s.registry == nil || len(s.registry.List()) == 0 {
		return nil
	}
	var names []string
	var native, comments, prComments, events bool
	for _, tracked := range s.registry.List() {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(tracked.ID())}); err != nil {
			continue
		}
		if names == nil {
			names = []string{operatortool.WorkList, operatortool.WorkItem, operatortool.WorkConfig, operatortool.WorkRelationships, operatortool.WorkReferences, operatortool.BoardActivity, operatortool.BoardSession}
		}
		if provider, ok := tracked.Connector().(nativeClientSource); ok && provider.NativeClient() != nil {
			native = true
		}
		if _, ok := tracked.Connector().(connector.IssueCommentReader); ok {
			comments = true
		}
		if _, ok := tracked.Connector().(connector.PullRequestCommentReader); ok {
			prComments = true
		}
		if _, ok := tracked.Connector().(connector.IssueEventReader); ok {
			events = true
		}
	}
	if names == nil {
		return nil
	}
	if native {
		names = append(names, operatortool.WorkVersion, operatortool.WorkExport)
	}
	if native || comments {
		names = append(names, operatortool.WorkComments)
	}
	if prComments {
		names = append(names, operatortool.WorkPRComments)
	}
	if native || events || s.store != nil {
		names = append(names, operatortool.WorkHistory)
	}
	if native || s.store != nil {
		names = append(names, operatortool.WorkRuns)
	}
	if native || s.store != nil {
		names = append(names, operatortool.BoardReceipt)
	}
	if native || s.store != nil && s.history != nil {
		names = append(names, operatortool.BoardSessionHistory)
	}
	if native || s.recovery != nil {
		names = append(names, operatortool.WorkAttemptReceipt)
	}
	if native {
		names = append(names, operatortool.GitHubScopeTimings)
	}
	return names
}

func (r dashboardWorkReads) ReadWork(ctx context.Context, name string, request operatortool.WorkReadRequest) (operatortool.Result, error) {
	s := r.server
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: request.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	if s.registry == nil {
		return operatortool.Result{}, operatortool.ErrReadUnavailable
	}
	tracked, ok := s.registry.Get(project.ID(request.ProjectID))
	if !ok {
		return operatortool.Result{}, explain.ErrNotFound
	}
	snapshot, err := operatortool.ProjectSnapshot(ctx, s.chatSnapshot(ctx))
	if err != nil {
		return operatortool.Result{}, err
	}
	var client *hubclient.NativeClient
	if source, ok := tracked.Connector().(nativeClientSource); ok {
		client = source.NativeClient()
	}
	if client != nil {
		switch name {
		case operatortool.WorkList, operatortool.WorkItem, operatortool.WorkConfig, operatortool.WorkComments, operatortool.WorkPRComments, operatortool.WorkHistory, operatortool.WorkVersion, operatortool.WorkRelationships, operatortool.WorkRuns, operatortool.WorkReferences, operatortool.WorkExport, operatortool.BoardActivity, operatortool.BoardReceipt, operatortool.BoardSession, operatortool.BoardSessionHistory, operatortool.WorkAttemptReceipt, operatortool.GitHubScopeTimings:
			return s.readNativeWork(ctx, client, tracked, request, name)
		}
	}
	if snapshot.GeneratedAt.IsZero() {
		return operatortool.Result{}, operatortool.ErrSnapshotUnavailable
	}
	if name == operatortool.WorkList {
		if request.Cursor != "" || request.Archived != "" || request.Assignee != "" || len(request.Assignees) != 0 || request.Priority != nil || len(request.Priorities) != 0 || len(request.Include) != 0 {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		issues := scopedWorkIssues(snapshot, request.ProjectID)
		states := append([]string{}, request.States...)
		if request.State != "" {
			states = append(states, request.State)
		}
		labels := append([]string{}, request.Labels...)
		if request.Label != "" {
			labels = append(labels, request.Label)
		}
		issues = slices.DeleteFunc(issues, func(issue telemetry.Issue) bool {
			return len(states) != 0 && !slices.ContainsFunc(states, func(state string) bool { return strings.EqualFold(issue.State, state) }) || request.Query != "" && !strings.Contains(strings.ToLower(issue.Title+" "+issue.Description+" "+issue.Identifier), strings.ToLower(request.Query)) || len(labels) != 0 && !slices.ContainsFunc(labels, func(label string) bool { return slices.Contains(issue.Labels, label) })
		})
		return snapshotWorkResult(request, snapshot, operatortool.OffsetPage(issues, request.Offset, request.Limit))
	}
	if name == operatortool.WorkConfig {
		labels := []string{}
		for _, issue := range scopedWorkIssues(snapshot, request.ProjectID) {
			labels = append(labels, issue.Labels...)
		}
		slices.Sort(labels)
		labels = slices.Compact(labels)
		return snapshotWorkResult(request, snapshot, struct {
			Lanes      []string       `json:"lanes"`
			Priorities map[string]any `json:"priorities"`
			Labels     []string       `json:"labels"`
		}{tracked.Workflow().Config.KanbanStateNames(), tracked.Workflow().Config.Tracker.PriorityMap.Map, labels})
	}
	selected, err := explain.ResolveSnapshotIssue(snapshot, explain.Query{ProjectID: request.ProjectID, Reference: request.Reference}, explain.SnapshotIssueScope{IncludeCompleted: true, IncludeTrackerDrift: true})
	if err != nil {
		return operatortool.Result{}, err
	}
	issue := selected.Issue
	request.Reference = issue.ID
	switch name {
	case operatortool.WorkItem:
		detail, err := issueResponse(issue.ID, request.ProjectID, snapshot)
		if err != nil {
			return operatortool.Result{}, err
		}
		return snapshotWorkResult(request, snapshot, struct {
			Issue   telemetry.Issue  `json:"issue"`
			Runtime issueAPIResponse `json:"runtime"`
		}{issue, detail})
	case operatortool.WorkComments:
		if request.Cursor != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		comments, err := readIssueDiscussion(ctx, tracked.Connector(), connectorIssueForWork(issue))
		if err != nil {
			return operatortool.Result{}, err
		}
		return nativeWorkResult(request, operatortool.OffsetPage(comments, request.Offset, request.Limit), nil)
	case operatortool.WorkPRComments:
		if request.Cursor != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		if issue.PullRequest == nil || issue.PullRequest.Number <= 0 {
			return operatortool.Result{}, explain.ErrNotFound
		}
		comments, err := readPRDiscussion(ctx, tracked.Connector(), kanbanPullRequestRepository(issue), issue.PullRequest.Number)
		return nativeWorkResult(request, operatortool.OffsetPage(comments, request.Offset, request.Limit), err)
	case operatortool.WorkHistory:
		if request.Cursor != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		reader, ok := tracked.Connector().(connector.IssueEventReader)
		if !ok {
			if s.store == nil {
				return operatortool.Result{}, operatortool.ErrReadUnavailable
			}
			timeline, err := s.store.IssueWorkflowTimeline(ctx, store.IssueIdentity{ProjectID: request.ProjectID, IssueID: issue.ID, Identifier: issue.Identifier})
			if err != nil {
				return operatortool.Result{}, err
			}
			return snapshotWorkResult(request, snapshot, operatortool.HistoryPage[store.WorkflowPhaseEvent]{ReadPage: operatortool.OffsetPage(timeline.Events, request.Offset, request.Limit), Source: "workflow"})
		}
		events, err := reader.FetchIssueEvents(ctx, connectorIssueForWork(issue))
		if err != nil {
			return operatortool.Result{}, err
		}
		return nativeWorkResult(request, operatortool.HistoryPage[connector.IssueEvent]{ReadPage: operatortool.OffsetPage(events, request.Offset, request.Limit), Source: "tracker"}, nil)
	case operatortool.WorkRelationships:
		// Dependencies without project ownership are retained only when their
		// identity appears in this already authorized snapshot projection.
		return snapshotWorkResult(request, snapshot, struct {
			Dependencies []telemetry.BlockedRef `json:"dependencies"`
		}{issue.BlockedBy})
	case operatortool.WorkRuns:
		if request.Cursor != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		if s.store == nil {
			return operatortool.Result{}, operatortool.ErrReadUnavailable
		}
		runs, err := s.store.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{ProjectID: request.ProjectID, IssueID: issue.ID, Identifier: issue.Identifier, Limit: request.Offset + request.Limit + 1})
		if err != nil {
			return operatortool.Result{}, err
		}
		return snapshotWorkResult(request, snapshot, operatortool.OffsetPage(runs, request.Offset, request.Limit))
	case operatortool.WorkReferences:
		if request.Cursor != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		refs := []operatortool.WorkReference{}
		if number := pullRequestNumber(issue); number != nil {
			refs = append(refs, operatortool.WorkReference{Kind: "pull_request", ID: strconv.Itoa(*number), URL: pullRequestURL(issue)})
		}

		if issue.Deliverable != nil {
			refs = append(refs, operatortool.WorkReference{Kind: issue.Deliverable.Kind, ID: issue.Deliverable.ExternalID, URL: issue.Deliverable.ReviewURL})
		}
		return snapshotWorkResult(request, snapshot, operatortool.OffsetPage(refs, request.Offset, request.Limit))
	case operatortool.BoardActivity:
		data := s.boardActivityData(ctx, snapshot, issue, boardActivityRequest{ProjectID: request.ProjectID, Issue: issue.ID, Identifier: issue.Identifier, Limit: request.Limit})
		if data.Error != "" {
			return operatortool.Result{}, operatortool.ErrReadUnavailable
		}
		return snapshotWorkResult(request, snapshot, data)
	case operatortool.BoardReceipt:
		if s.store == nil {
			return operatortool.Result{}, operatortool.ErrReadUnavailable
		}
		receipt, err := s.store.EfficiencyReceipt(ctx, request.ProjectID, issue.ID, issue.Identifier)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return operatortool.Result{}, err
		}
		return snapshotWorkResult(request, snapshot, struct {
			Available bool                `json:"available"`
			Receipt   *efficiency.Receipt `json:"receipt"`
		}{err == nil, func() *efficiency.Receipt {
			if err != nil {
				return nil
			}
			return &receipt
		}()})
	case operatortool.BoardSession:
		return snapshotWorkResult(request, snapshot, s.boardSessionData(ctx, snapshot, issue, request.ProjectID))
	case operatortool.BoardSessionHistory:
		data := s.readBoardSessionHistory(ctx, snapshot, issue, request.ProjectID, request.Offset, request.Limit)
		if data.Error != "" {
			return operatortool.Result{}, operatortool.ErrReadUnavailable
		}
		return snapshotWorkResult(request, snapshot, data)
	case operatortool.WorkAttemptReceipt:
		if s.recovery == nil {
			return operatortool.Result{}, operatortool.ErrReadUnavailable
		}
		receipt, err := s.recovery.WorkAttemptReceipt(ctx, request.ProjectID, request.AttemptID)
		if err != nil {
			return operatortool.Result{}, err
		}
		if receipt.Attempt.AttemptID != request.AttemptID || receipt.Attempt.ProjectID != request.ProjectID || receipt.Attempt.IssueID != issue.ID {
			return operatortool.Result{}, explain.ErrNotFound
		}
		return snapshotWorkResult(request, snapshot, receipt)
	default:
		return operatortool.Result{}, operatortool.ErrReadUnavailable
	}
}

func snapshotWorkResult[T any](request operatortool.WorkReadRequest, snapshot telemetry.Snapshot, data T) (operatortool.Result, error) {
	freshness := explain.SourceLive
	if snapshot.LastKnown {
		freshness = explain.SourceLastKnown
	}
	var expires *time.Time
	if !snapshot.LastKnownUntil.IsZero() {
		at := snapshot.LastKnownUntil
		expires = &at
		if time.Now().After(at) {
			freshness = explain.SourceExpired
		}
	}
	return operatortool.EncodeResult(operatortool.WorkReadResult[T]{ProjectID: request.ProjectID, Reference: request.Reference, URL: operatortool.WorkItemURL(request.ProjectID, request.Reference), GeneratedAt: snapshot.GeneratedAt, Freshness: freshness, ExpiresAt: expires, Data: data})
}

func scopedWorkIssues(snapshot telemetry.Snapshot, projectID string) []telemetry.Issue {
	rows := map[string]telemetry.Issue{}
	add := func(issue telemetry.Issue) {
		if issue.ProjectID == projectID {
			rows[issue.ID] = issue
		}
	}
	for _, issue := range snapshot.BoardIssues {
		add(issue)
	}
	for _, issue := range snapshot.Pipeline {
		add(issue)
	}
	for _, issue := range snapshot.Running {
		add(issue.Issue)
	}
	for _, issue := range snapshot.Queue {
		add(issue.Issue)
	}
	for _, issue := range snapshot.Completed {
		add(issue.Issue)
	}
	issues := make([]telemetry.Issue, 0, len(rows))
	for _, issue := range rows {
		issues = append(issues, issue)
	}
	slices.SortFunc(issues, func(a, b telemetry.Issue) int { return strings.Compare(a.Identifier, b.Identifier) })
	return issues
}

func connectorIssueForWork(issue telemetry.Issue) connector.Issue {
	return connector.Issue{ID: issue.ID, Identifier: issue.Identifier, Number: issue.Number, Title: issue.Title, Description: issue.Description, URL: issue.URL, State: issue.State, PRNumber: pullRequestNumber(issue)}
}

func (s *Server) readNativeWork(ctx context.Context, client *hubclient.NativeClient, tracked *project.Project, request operatortool.WorkReadRequest, name string) (operatortool.Result, error) {
	if request.Offset != 0 && name != operatortool.WorkReferences && name != operatortool.BoardSessionHistory && name != operatortool.WorkPRComments {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}

	if name == operatortool.WorkList {
		page, err := client.IssuesPage(ctx, request.NativeWorkQuery())
		if err == nil {
			for i := range page.Items {
				page.Items[i], err = s.projectNativeWork(ctx, client, page.Items[i])
				if err != nil {
					break
				}
			}
			if err == nil && page.Work != nil {
				for i := range page.Work.Items {
					page.Work.Items[i], err = s.projectNativeWork(ctx, client, page.Work.Items[i])
					if err != nil {
						break
					}
				}
			}
		}
		return nativeWorkResult(request, operatortool.NativeWorkPageView(request.ProjectID, page), err)
	}
	if name == operatortool.WorkConfig {
		p, err := client.Project(ctx)
		if err != nil {
			return operatortool.Result{}, nativeWorkReadError(err)
		}
		labels, err := client.Labels(ctx)
		return nativeWorkResult(request, struct {
			Project    tracker.NativeProject `json:"project"`
			Priorities []int                 `json:"priorities"`
			Labels     []tracker.NativeLabel `json:"labels"`
		}{p, []int{0, 1, 2, 3}, labels}, err)
	}
	id := tracker.NativeWorkItemID(request.Reference)
	if !strings.HasPrefix(request.Reference, "wi_") {
		// The same project snapshot identity resolver used by the dashboard.
		snapshot, err := operatortool.ProjectSnapshot(ctx, s.chatSnapshot(ctx))
		if err != nil {
			return operatortool.Result{}, err
		}
		selected, err := explain.ResolveSnapshotIssue(snapshot, explain.Query{ProjectID: request.ProjectID, Reference: request.Reference}, explain.SnapshotIssueScope{IncludeCompleted: true})
		if err != nil {
			return operatortool.Result{}, err
		}
		id = tracker.NativeWorkItemID(selected.Issue.ID)
	}
	issue, err := client.Issue(ctx, id)
	if err != nil {
		return operatortool.Result{}, nativeWorkReadError(err)
	}
	issue, err = s.projectNativeWork(ctx, client, issue)
	if err != nil {
		return operatortool.Result{}, err
	}
	request.Reference = string(id)
	switch name {
	case operatortool.GitHubScopeTimings:
		data, err := client.GitHubTimings(ctx, id, request.NativeAttemptID, request.RunnerID)
		if err != nil {
			return operatortool.Result{}, nativeWorkReadError(err)
		}
		if data.ProjectID != client.ProjectID() || data.WorkItemID != id || data.AttemptID != request.NativeAttemptID || data.RunnerID != request.RunnerID {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		return nativeWorkResult(request, data, nil)
	case operatortool.BoardReceipt, operatortool.BoardSession, operatortool.BoardSessionHistory, operatortool.WorkAttemptReceipt:
		if request.Cursor != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		selector := request.NativeAttemptID
		if request.AttemptID > 0 {
			selector = strconv.FormatInt(request.AttemptID, 10)
		}
		evidence, err := client.RuntimeEvidence(ctx, id, selector)
		if err != nil {
			return operatortool.Result{}, nativeWorkReadError(err)
		}
		if evidence.Issue.ProjectID != client.ProjectID() || evidence.Issue.WorkItemID != id {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		evidence.Issue, err = s.projectNativeWork(ctx, client, evidence.Issue)
		if err != nil {
			return operatortool.Result{}, err
		}
		return operatortool.NativeRuntimeResult(name, request, evidence)
	case operatortool.WorkItem:
		return nativeWorkResult(request, operatortool.NativeItemView(request.ProjectID, issue), nil)
	case operatortool.WorkExport:
		return nativeWorkResult(request, issue, nil)
	case operatortool.WorkPRComments:
		return operatortool.Result{}, operatortool.ErrReadUnavailable
	case operatortool.WorkRelationships:
		return nativeWorkResult(request, struct {
			Dependencies []tracker.NativeWorkItemID  `json:"dependencies"`
			Blockers     []tracker.NativeDependency  `json:"blockers"`
			References   []tracker.ExternalReference `json:"references"`
		}{issue.Dependencies, issue.Blockers, issue.ExternalReferences}, nil)
	case operatortool.WorkComments:
		page, err := client.CommentsPage(ctx, id, request.Cursor, request.Limit)
		return nativeWorkResult(request, page, err)
	case operatortool.WorkHistory, operatortool.BoardActivity:
		page, err := client.HistoryPage(ctx, id, request.Cursor, request.Limit)
		if err == nil {
			for i := range page.Items {
				related := page.Items[i].Data.RelatedWorkItemID
				if related == "" {
					continue
				}
				visible, readErr := s.nativeRelatedVisible(ctx, client, related)
				if readErr != nil {
					err = readErr
					break
				}
				if !visible {
					page.Items[i].Data.RelatedWorkItemID = ""
				}
			}
		}
		return nativeWorkResult(request, page, err)
	case operatortool.WorkVersion:
		version, err := client.Version(ctx, id, request.CommentID, request.Revision)
		if err == nil && request.CommentID == "" {
			var saved tracker.NativeIssue
			if err = json.Unmarshal(version, &saved); err == nil {
				saved, err = s.projectNativeWork(ctx, client, saved)
				if err == nil {
					version, err = json.Marshal(saved)
				}
			}
		}
		return nativeWorkResult(request, version, err)
	case operatortool.WorkRuns:
		page, err := client.AttemptsPage(ctx, id, request.Cursor, request.Limit)
		return nativeWorkResult(request, page, err)
	case operatortool.WorkReferences:
		reader, ok := tracked.Connector().(tracker.ChangeReader)
		if !ok {
			return operatortool.Result{}, operatortool.ErrReadUnavailable
		}
		changes, err := reader.FetchChanges(ctx, id)
		if err != nil {
			return operatortool.Result{}, nativeWorkReadError(err)
		}
		refs := []operatortool.WorkReference{}
		if request.Cursor != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		for _, change := range changes {
			refs = append(refs, operatortool.WorkReference{Kind: "change", ID: change.ID, VersionID: change.CurrentVersion, URL: "/projects/" + url.PathEscape(request.ProjectID) + "/issues/" + url.PathEscape(string(id)) + "/changes/" + url.PathEscape(change.ID)})
		}
		artifacts, err := client.Artifacts(ctx, id)
		if err != nil {
			return operatortool.Result{}, nativeWorkReadError(err)
		}
		for _, ref := range artifacts {
			refs = append(refs, operatortool.ArtifactWorkReference(request.ProjectID, string(id), ref))
		}
		prs, err := client.PullRequestReferences(ctx, id)
		if err != nil {
			return operatortool.Result{}, nativeWorkReadError(err)
		}
		for _, pr := range prs {
			if pr.Number > 0 && pr.URL != "" {
				refs = append(refs, operatortool.WorkReference{Kind: "pull_request", ID: strconv.Itoa(pr.Number), URL: pr.URL, ObservedAt: &pr.FetchedAt})
			}
		}
		return nativeWorkResult(request, operatortool.OffsetPage(refs, request.Offset, request.Limit), nil)
	default:
		return operatortool.Result{}, operatortool.ErrReadUnavailable
	}
}

// The hub credential belongs to the configured connector, not the caller.
// Project each related resource through current local application grants too.
func (s *Server) nativeWorkClients(ctx context.Context, source *hubclient.NativeClient) []*hubclient.NativeClient {
	var clients []*hubclient.NativeClient
	for _, tracked := range s.registry.List() {
		provider, ok := tracked.Connector().(nativeClientSource)
		if !ok {
			continue
		}
		client := provider.NativeClient()
		if !source.SameOrganization(client) {
			continue
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(tracked.ID())}); err == nil {
			clients = append(clients, client)
		}
	}
	return clients
}

func (s *Server) nativeRelatedVisible(ctx context.Context, source *hubclient.NativeClient, id tracker.NativeWorkItemID) (bool, error) {
	for _, client := range s.nativeWorkClients(ctx, source) {
		if _, err := client.Issue(ctx, id); err == nil {
			return true, nil
		} else if !errors.Is(nativeWorkReadError(err), explain.ErrNotFound) {
			return false, nativeWorkReadError(err)
		}
	}
	return false, nil
}

func (s *Server) projectNativeWork(ctx context.Context, source *hubclient.NativeClient, issue tracker.NativeIssue) (tracker.NativeIssue, error) {
	if len(issue.Dependencies) == 0 && len(issue.Blockers) == 0 {
		return issue, nil
	}
	clients := s.nativeWorkClients(ctx, source)
	visible := make(map[tracker.NativeWorkItemID]bool, len(issue.Dependencies))
	known := make(map[tracker.NativeWorkItemID]bool, len(issue.Blockers))
	for _, blocker := range issue.Blockers {
		known[blocker.ID] = true
		for _, client := range clients {
			if client.ProjectID() == blocker.ProjectID {
				visible[blocker.ID] = true
				break
			}
		}
	}
	for _, id := range issue.Dependencies {
		if known[id] {
			continue
		}
		allowed, err := s.nativeRelatedVisible(ctx, source, id)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		visible[id] = allowed
	}
	issue.Dependencies = slices.DeleteFunc(slices.Clone(issue.Dependencies), func(id tracker.NativeWorkItemID) bool { return !visible[id] })
	issue.Blockers = slices.DeleteFunc(slices.Clone(issue.Blockers), func(blocker tracker.NativeDependency) bool { return !visible[blocker.ID] })
	return issue, nil
}

func nativeWorkReadError(err error) error {
	if errors.Is(err, hubclient.ErrUnavailable) {
		return operatortool.ErrReadUnavailable
	}
	var api *hubclient.APIError
	if errors.As(err, &api) {
		switch api.Status {
		case 404:
			return explain.ErrNotFound
		case 400, 422:
			return operatortool.ErrInvalidArguments
		case 401, 403:
			return operatortool.ErrAccessDenied
		}
	}
	return operatortool.ErrReadUnavailable
}
func nativeWorkResult[T any](request operatortool.WorkReadRequest, data T, err error) (operatortool.Result, error) {
	if err != nil {
		return operatortool.Result{}, nativeWorkReadError(err)
	}
	return operatortool.EncodeResult(operatortool.WorkReadResult[T]{ProjectID: request.ProjectID, Reference: request.Reference, URL: operatortool.WorkItemURL(request.ProjectID, request.Reference), GeneratedAt: time.Now().UTC(), Freshness: explain.SourceAvailable, Data: data})
}
