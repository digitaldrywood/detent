package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func boardRecordSelection(scope hubscope.NativeScope, params url.Values) (viewstore.BoardSelection, error) {
	includeWorkspace, err := parseNativeIssueIncludes(params.Get("include"))
	if err != nil {
		return viewstore.BoardSelection{}, err
	}
	selection := viewstore.BoardSelection{ProjectID: scope.Project, Reporter: scope.Credential.HostedRole == "reporter", ReporterPrincipalID: scope.Actor().PrincipalID, States: params["state"], Types: params["type"], Verification: params["verification"], Labels: params["label"], Assignees: params["assignee"], ClosedOrder: params.Get("sort") == "closed", ExcludeWorkspace: !includeWorkspace, Fingerprint: strings.TrimSpace(params.Get("fingerprint"))}
	for _, values := range []*[]string{&selection.States, &selection.Types, &selection.Verification, &selection.Labels, &selection.Assignees} {
		*values = slices.DeleteFunc(slices.Clone(*values), func(value string) bool { return value == "" })
	}
	selection.ActorPrincipalID, err = nativeCreatedBy(scope, params)
	if err != nil {
		return selection, err
	}
	switch params.Get("archived") {
	case "", "false":
		selection.Archived = new(false)
	case "true":
		selection.Archived = new(true)
	case "all":
	default:
		return selection, hubapi.NativePublicInvalid("archived must be true, false or all")
	}
	switch params.Get("open") {
	case "":
	case "true":
		selection.Terminal = new(false)
	case "false":
		selection.Terminal = new(true)
	default:
		return selection, hubapi.NativePublicInvalid("open must be true or false")
	}
	for _, value := range params["priority"] {
		if value == "" {
			continue
		}
		priority, err := strconv.Atoi(value)
		if err != nil || priority < 0 || priority > 3 {
			return selection, hubapi.NativePublicInvalid("priority must be between zero and three")
		}
		selection.Priorities = append(selection.Priorities, priority)
	}
	for _, bound := range []struct {
		name  string
		value **time.Time
	}{{"completed_from", &selection.CompletedFrom}, {"completed_to", &selection.CompletedTo}} {
		if value := params.Get(bound.name); value != "" {
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return selection, hubapi.NativePublicInvalid("Completion window bounds must be RFC3339 timestamps")
			}
			*bound.value = &at
		}
	}
	for _, bound := range []struct {
		name  string
		value **time.Time
	}{{"created_after", &selection.NativeCreatedAfter}, {"created_before", &selection.NativeCreatedBefore}, {"updated_after", &selection.NativeUpdatedAfter}} {
		if value := params.Get(bound.name); value != "" {
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return selection, hubapi.NativePublicInvalid("Date filters must be RFC3339 timestamps")
			}
			*bound.value = &at
		}
	}
	return selection, nil
}

func boardRecordCursor(row viewstore.BoardRecord, closed bool, sequence int64) *nativeIssuePageCursor {
	position := &nativeIssuePageCursor{Sequence: &sequence, Score: row.Position.SearchRank, SearchExact: row.Position.SearchExact, SearchNumber: row.Position.SearchNumber, Lane: row.Position.Lane, State: row.Position.State, Priority: row.Position.Priority, Identifier: row.Position.Identifier}
	if row.Position.ActivityMissing {
		position.ActivityMissing = 1
	}
	at := row.Position.Activity
	if closed {
		at = row.ClosedAt
	}
	if at != nil {
		position.Activity = at.UTC().Format(time.RFC3339Nano)
	}
	return position
}

func nativeWorkCursorParams(params url.Values) url.Values {
	cursorParams := url.Values{}
	for name, values := range params {
		cursorParams[name] = values
	}
	includes := slices.DeleteFunc(strings.Split(params.Get("include"), ","), func(name string) bool {
		return slices.Contains([]string{"", "summary", "work"}, strings.TrimSpace(name))
	})
	cursorParams.Del("include")
	if len(includes) > 0 {
		cursorParams.Set("include", strings.Join(includes, ","))
	}
	return cursorParams
}

func (s *Service) readBoardRecords(ctx context.Context, scope hubscope.NativeScope, params url.Values) (tracker.NativeIssuePage, error) {
	return s.readBoardRecordsFor(ctx, scope, params, false)
}

func (s *Service) readBoardRecordsFor(ctx context.Context, scope hubscope.NativeScope, params url.Values, legacy bool) (tracker.NativeIssuePage, error) {
	return s.readBoardProjection(ctx, scope, params, legacy, false, false)
}

func (s *Service) readBoardProjection(ctx context.Context, scope hubscope.NativeScope, params url.Values, legacy, compact, lanes bool) (tracker.NativeIssuePage, error) {
	if value, present := params["archive_url"]; present && (len(value) != 1 || value[0] != "true" && value[0] != "false") {
		return tracker.NativeIssuePage{}, hubapi.NativePublicInvalid("archive_url supports true or false")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return tracker.NativeIssuePage{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	var page tracker.NativeIssuePage
	var compatible bool
	read := func(ctx context.Context) error {
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			project, err := r.Project(ctx, scope.Project)
			if err != nil {
				return err
			}
			if project.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			compatible = project.Profile == "github_compatible"
			if !compatible && project.Profile != "native" {
				return corestore.ErrNotFound
			}
			return nil
		}); err != nil {
			return hubapi.ProjectRecordError(err)
		}
		if compatible {
			return nil
		}
		if err := hubrecords.ValidateNativeIssueQuery(params); err != nil {
			return err
		}
		var err error
		page, err = s.readBoardRecordSnapshot(ctx, scope, params, legacy, compact, lanes)
		return err
	}
	var err error
	if repository, ok := s.config.TenantRepository.(interface {
		ReadBoardRequest(context.Context, tracker.OrganizationID, string, func(context.Context) error) error
	}); ok {
		err = repository.ReadBoardRequest(ctx, scope.Organization, scope.Credential.ID, read)
	} else if repository, ok := s.config.TenantRepository.(tenantstore.RequestReader); ok {
		err = repository.ReadRequest(ctx, scope.Organization, read)
	} else {
		err = read(ctx)
	}
	if err == nil && compatible {
		return s.readQueueWorkItems(ctx, scope, params)
	}
	return page, err
}

func (s *Service) readBoardRecordSnapshot(ctx context.Context, scope hubscope.NativeScope, params url.Values, legacy, compact, lanes bool) (tracker.NativeIssuePage, error) {
	page := tracker.NativeIssuePage{Page: tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}}}
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return page, hubapi.NativeWorkItemStorageUnavailable()
	}
	if _, err := nativeIssuePickerRequested(params); err != nil {
		return page, err
	}
	includeBoard := slices.ContainsFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return strings.TrimSpace(name) == "board" })
	includeWork := slices.ContainsFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return strings.TrimSpace(name) == "work" })
	includeSummary := slices.ContainsFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return strings.TrimSpace(name) == "summary" })
	if includeSummary && includeWork {
		return page, hubapi.NativePublicInvalid("summary cannot include work")
	}
	if includeBoard {
		page.Cards = []tracker.NativeBoardCard{}
		if params.Get("include") != "board" {
			return page, hubapi.NativePublicInvalid("board cannot include other projections")
		}
		if params.Has("created_after") || params.Has("created_before") || params.Has("updated_after") {
			return page, hubapi.NativePublicInvalid("Date filters require an issue list projection")
		}
	}
	if sort := params.Get("sort"); sort != "" && sort != "closed" {
		return page, hubapi.NativePublicInvalid("sort supports closed")
	}
	selection, err := boardRecordSelection(scope, params)
	if err != nil {
		return page, err
	}
	selection.SearchText = strings.TrimSpace(params.Get("q"))
	selection.SearchRanked = !includeBoard && selection.SearchText != ""
	if len(selection.SearchText) > searchstore.MaxQueryTextBytes {
		return page, hubapi.NativePublicInvalid("Search query is too long")
	}
	selectedWindow, err := nativeShippedWindow(params.Get("completed_window"))
	if err != nil {
		return page, err
	}
	path, maximum := "/api/v2/organizations/"+url.PathEscape(string(scope.Organization))+"/projects/"+url.PathEscape(string(scope.Project))+"/work-items", hubapi.MaxAPIPageLimit
	if includeBoard {
		path, maximum = "/board/"+string(scope.Project), 2000
	}
	cursorParams := params
	if !includeBoard {
		cursorParams = nativeWorkCursorParams(params)
	}
	limit, cursor, key, err := s.readNativePageLimit(ctx, scope, path, cursorParams, maximum, hubapi.DefaultAPIPageLimit)
	if err != nil {
		return page, err
	}
	if !legacy {
		limit = min(limit, 200)
	}
	selection.Summary = (legacy || lanes && includeWork) && selection.SupportsSummary()
	selection.Limit = limit + 1
	selection.Compact = includeSummary || includeWork
	if position := cursor.Issues; position != nil {
		selection.After = &viewstore.BoardPosition{SearchExact: position.SearchExact, SearchRank: position.Score, SearchNumber: position.SearchNumber, Lane: position.Lane, State: position.State, Priority: position.Priority, ActivityMissing: position.ActivityMissing != 0, Identifier: position.Identifier}
		selection.AfterSequence = position.Sequence
		if position.Activity != "" {
			at, err := time.Parse(time.RFC3339Nano, position.Activity)
			if err != nil {
				return page, corestore.ErrInvalidCursor
			}
			selection.After.Activity = &at
		}
	}
	var snapshot viewstore.BoardModelPage
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		if includeBoard {
			page.GrantVersion, err = s.boardAccessVersion(ctx, reader, scope)
			if err != nil {
				return err
			}
		}
		model, ok := reader.(interface {
			BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
		})
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var work *viewstore.BoardSelection
		if includeWork {
			value := selection
			value.After = nil
			value.WorkOrder = true
			value.Terminal = new(false)
			work = &value
		}
		snapshot, err = model.BoardModelPages(ctx, selection, work, s.accessAuthority(scope, false))
		if legacy && snapshot.Pending && err == nil && selection.SearchText == "" {
			return searchstore.ErrIndexPending
		}
		if err != nil {
			return err
		}
		return s.authorizeBoardModel(ctx, reader, scope, &snapshot)
	})
	if err != nil {
		return page, hubapi.ProjectRecordError(err)
	}
	if selection.AfterSequence != nil && *selection.AfterSequence != snapshot.Sequence {
		return page, hubapi.NativePublicInvalid("Work list order changed; restart the query")
	}
	page.ProjectionPending, page.ProjectionOldestPendingAt = snapshot.Pending, snapshot.OldestPendingAt
	page.ChangeSequence = snapshot.Sequence
	if includeBoard {
		page.Sequence = snapshot.Sequence
	}
	page.Total = -1
	if (includeBoard || includeWork) && (!legacy || selection.Summary) {
		page.Work = &tracker.NativeWorkSummary{Items: []tracker.NativeIssue{}, Lanes: []tracker.NativeWorkLane{}, ShippedWindows: []tracker.NativeShippedWindow{}, AsOf: s.config.Clock()}
	}
	if selection.Summary {
		page.Total = 0
		windows := []tracker.NativeShippedWindow{{Window: "1h"}, {Window: "24h"}, {Window: "7d"}}
		filterCompletionStates := !includeBoard && len(selection.States) > 0
		for _, summary := range snapshot.Summaries {
			if summary.Kind == "lane" && (!lanes || len(selection.States) == 0 || slices.Contains(selection.States, summary.State)) {
				page.Work.Lanes = append(page.Work.Lanes, tracker.NativeWorkLane{State: summary.State, Total: int(summary.Total), Running: int(summary.Running)})
				if len(selection.States) > 0 && slices.Contains(selection.States, summary.State) {
					page.Total += int(summary.Total)
				}
			}
			if summary.Kind == "total" && !filterCompletionStates || summary.Kind == "lane" && filterCompletionStates && slices.Contains(selection.States, summary.State) {
				windows[0].Shipped += int(summary.Shipped1h)
				windows[0].Cancelled += int(summary.Cancelled1h)
				windows[1].Shipped += int(summary.Shipped24h)
				windows[1].Cancelled += int(summary.Cancelled24h)
				windows[2].Shipped += int(summary.Shipped7d)
				windows[2].Cancelled += int(summary.Cancelled7d)
			}
			if summary.Kind == "total" {
				page.Work.AsOf = summary.ProjectedAt
			}
			kind := "total"
			if selection.Terminal != nil {
				kind = "open"
				if *selection.Terminal {
					kind = "closed"
				}
			}
			if len(selection.States) == 0 && summary.Kind == kind {
				page.Total = int(summary.Total)
			}
		}
		applyNativeShippedWindows(page.Work, windows, selectedWindow)
	}
	if err := s.hydrateBoardRecordActorNames(ctx, scope.Organization, snapshot.Records, snapshot.WorkRecords); err != nil {
		return page, err
	}
	if selection.SearchText != "" || selection.Fingerprint != "" {
		for _, records := range [][]viewstore.BoardRecord{snapshot.Records, snapshot.WorkRecords} {
			for index := range records {
				row := &records[index]
				issue := row.PageIssue
				if issue == nil {
					return page, viewstore.ErrInvalidView
				}
				if selection.Compact {
					compact := operatortool.NativeListIssue(*issue)
					issue = &compact
				}
				if selection.SearchText != "" {
					issue.Snippet = searchstore.Snippet(searchstore.Text{Title: row.PageIssue.Title, Body: row.PageIssue.Body}, selection.SearchText)
					issue.SearchScore = row.Position.SearchRank
					extra, err := json.Marshal(struct {
						Snippet     string  `json:"snippet"`
						SearchScore float64 `json:"search_score"`
					}{issue.Snippet, issue.SearchScore})
					if err != nil {
						return page, err
					}
					row.ListBytes += len(extra)
				}
				row.PageIssue = issue
			}
		}
	}
	records := snapshot.Records
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	returned := make([]viewstore.BoardRecord, 0, len(records))
	used := 0
	for _, row := range records {
		if selection.Compact && !compact && used+row.ListBytes > operatortool.WorkListPageBytes {
			if len(page.Items) == 0 {
				return page, operatortool.ErrResultTooLarge
			}
			hasMore = true
			break
		}
		card, err := boardModelCard(scope, row)
		if err != nil {
			return page, err
		}
		oversized := false
		if compact {
			card.Issue.LatestTransitionReason = row.Display.Reason
			card.Issue.OmittedFields = slices.DeleteFunc(slices.Clone(card.Issue.OmittedFields), func(field string) bool { return field == "latest_transition_reason" })
			candidate := tracker.NativeIssuePage{Page: tracker.Page[tracker.NativeIssue]{Items: append(slices.Clone(page.Items), card.Issue)}, Total: page.Total}
			next := cursor
			next.Issues = boardRecordCursor(row, selection.ClosedOrder, snapshot.Sequence)
			candidate.NextCursor, err = encodeNativeCursor(next, key)
			if err != nil {
				return page, err
			}
			raw, err := json.Marshal(operatortool.NativeSummaryPageView(candidate))
			if err != nil {
				return page, err
			}
			oversized = len(raw) > operatortool.WorkListPageBytes
			if oversized {
				if len(page.Items) > 0 {
					hasMore = true
					break
				}
				card.Issue, err = operatortool.BoundNativeListIssue(card.Issue)
				if err != nil {
					return page, err
				}
			}
		}
		used += row.ListBytes
		page.Items = append(page.Items, card.Issue)
		returned = append(returned, row)
		if includeBoard {
			page.Cards = append(page.Cards, card)
		}
		if oversized {
			hasMore = len(returned) < len(records) || hasMore
			break
		}
	}
	if includeWork {
		work := snapshot.WorkRecords
		page.Work.Truncated = len(work) > limit
		if page.Work.Truncated {
			work = work[:limit]
		}
		used = 0
		for _, row := range work {
			if used+row.ListBytes > operatortool.WorkListPageBytes {
				if len(page.Work.Items) == 0 {
					return page, operatortool.ErrResultTooLarge
				}
				page.Work.Truncated = true
				break
			}
			card, err := boardModelCard(scope, row)
			if err != nil {
				return page, err
			}
			used += row.ListBytes
			page.Work.Items = append(page.Work.Items, card.Issue)
		}
	}
	if hasMore {
		if len(returned) == 0 {
			return page, operatortool.ErrResultTooLarge
		}
		cursor.Issues = boardRecordCursor(returned[len(returned)-1], selection.ClosedOrder, snapshot.Sequence)
		page.NextCursor, err = encodeNativeCursor(cursor, key)
	}
	return page, err
}

func (s *Service) hydrateBoardRecordActorNames(ctx context.Context, organization tracker.OrganizationID, groups ...[]viewstore.BoardRecord) error {
	var issues []*tracker.NativeIssue
	var sizes []int
	for _, records := range groups {
		for index := range records {
			row := &records[index]
			if row.PageIssue == nil {
				return viewstore.ErrInvalidView
			}
			issue := *row.PageIssue
			row.PageIssue = &issue
			raw, err := json.Marshal(issue.Actor)
			if err != nil {
				return err
			}
			if row.ActorDisplayName == nil {
				issues = append(issues, row.PageIssue)
			} else if *row.ActorDisplayName != "" {
				row.PageIssue.Actor.DisplayName = *row.ActorDisplayName
			}
			sizes = append(sizes, len(raw))
		}
	}
	if err := s.hydrateWorkItemActorNames(ctx, organization, issues); err != nil {
		return err
	}
	position := 0
	for _, records := range groups {
		for index := range records {
			row := &records[index]
			raw, err := json.Marshal(row.PageIssue.Actor)
			if err != nil {
				return err
			}
			row.ListBytes += len(raw) - sizes[position]
			position++
		}
	}
	return nil
}

func boardModelCard(scope hubscope.NativeScope, row viewstore.BoardRecord) (tracker.NativeBoardCard, error) {
	if row.PageIssue == nil {
		return tracker.NativeBoardCard{}, viewstore.ErrInvalidView
	}
	issue := *row.PageIssue
	if scope.Credential.HostedRole == "reporter" {
		facts := row.ReporterRelations
		if facts == nil {
			return tracker.NativeBoardCard{}, viewstore.ErrInvalidView
		}
		issue.ReporterStatus = facts.ReporterStatus
		issue.Dependencies, issue.Blockers, issue.Dependents, issue.Related = facts.Dependencies, facts.Blockers, facts.Dependents, facts.Related
		issue.DependencyReady, issue.BlockedReason, issue.UnblocksReason = facts.DependencyReady, facts.BlockedReason, facts.UnblocksReason
	}
	issue = boardIssueCollections(issue)
	card := tracker.NativeBoardCard{Issue: issue, Attempt: row.Display.Attempt, Execution: row.Display.Execution, Change: row.Display.Change}
	card.StatusLine = nativeBoardStatusLine(card)
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: row.Display.ChangePrincipalID}) {
		card.Change = nil
		card.Issue.Change, card.Issue.ListChange, card.Issue.PullRequest = nil, nil, nil
	}
	return card, nil
}

func boardIssueCollections(issue tracker.NativeIssue) tracker.NativeIssue {
	for _, values := range []*[]string{&issue.Labels, &issue.Assignees} {
		if *values == nil {
			*values = []string{}
		}
	}
	if issue.Dependencies == nil {
		issue.Dependencies = []tracker.NativeWorkItemID{}
	}
	for _, values := range []*[]tracker.NativeDependency{&issue.Blockers, &issue.Dependents, &issue.Related} {
		if *values == nil {
			*values = []tracker.NativeDependency{}
		}
	}
	if issue.ExternalReferences == nil {
		issue.ExternalReferences = []tracker.ExternalReference{}
	}
	return issue
}

func (s *Service) boardAccessVersion(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope) (string, error) {
	authority := s.accessAuthority(scope, false)
	revisions, err := r.BoardAccessRevisions(ctx, authority.CredentialID, authority.UserID)
	if err != nil {
		return "", err
	}
	credential, err := r.AccessCredential(ctx, authority.CredentialID)
	if err != nil {
		return "", err
	}
	memberRole := ""
	if authority.UserID != "" {
		member, err := r.AccessMember(ctx, authority.UserID)
		if err != nil {
			return "", err
		}
		memberRole = member.Role
	}
	raw, err := json.Marshal([]any{scope.Organization, authority.CredentialID, authority.UserID, authority.Role, memberRole, credential.Scope, credential.ProjectAccess, credential.KeyScope, revisions, scope.Credential.EntryKey})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Service) readQueueWorkItems(ctx context.Context, scope hubscope.NativeScope, params url.Values) (tracker.NativeIssuePage, error) {
	page := tracker.NativeIssuePage{Page: tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}}, Total: -1}
	ctx = s.runDetailContext(ctx)
	ctx = context.WithValue(ctx, runtimeAdmissionPageKey{}, &runtimeAdmissionPage{})
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	allowed := map[string]bool{"limit": true, "cursor": true, "queue_scope": true, "readiness": true, "sort": true, "state": true, "label": true, "assignee": true, "priority": true, "repository": true}
	for name, values := range params {
		multi := name == "state" || name == "label" || name == "assignee" || name == "priority" || name == "repository"
		if !allowed[name] || len(values) == 0 || len(values) > 32 || !multi && len(values) != 1 || len(strings.Join(values, "\x00")) > 4096 {
			return page, hubapi.NativePublicInvalid("Query contains an unsupported field or value")
		}
	}
	limit, cursor, key, err := s.readNativePageLimit(ctx, scope, "/api/v2/organizations/"+string(scope.Organization)+"/projects/"+string(scope.Project)+"/work-items", params, corestore.MaxPageSize, hubapi.DefaultAPIPageLimit)
	if err != nil {
		return page, err
	}
	defer clear(key)
	query := claimstore.QueueListQuery{ProjectID: scope.Project, Scope: "fleet", States: params["state"], Labels: params["label"], Assignees: params["assignee"], Repositories: params["repository"], Limit: corestore.MaxPageSize + 1}
	if params.Get("queue_scope") == "project" || params.Get("queue_scope") == string(scope.Project) {
		query.Scope = string(scope.Project)
	} else if value := params.Get("queue_scope"); value != "" && value != "fleet" {
		return page, hubapi.NativePublicInvalid("queue_scope supports fleet or project")
	}
	switch params.Get("sort") {
	case "", "priority":
	case "identifier":
		query.IdentifierOrder = true
	default:
		return page, hubapi.NativePublicInvalid("sort supports priority or identifier")
	}
	readiness := params.Get("readiness")
	if readiness != "" && readiness != "ready" && readiness != "waiting" {
		return page, hubapi.NativePublicInvalid("readiness supports ready or waiting")
	}
	for _, value := range params["priority"] {
		priority, ok := map[string]int64{"urgent": 0, "high": 1, "normal": 2, "low": 3, "unset": -1}[value]
		if !ok {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 || parsed > 3 {
				return page, hubapi.NativePublicInvalid("priority is invalid")
			}
			priority = parsed
		}
		query.Priorities = append(query.Priorities, priority)
	}
	if scope.Credential.HostedRole == "reporter" {
		query.Principal = scope.Actor().PrincipalID
	}
	if cursor.After != "" {
		var envelope hubsecrets.Envelope
		if json.Unmarshal([]byte(cursor.After), &envelope) != nil || s.config.SecretKeys == nil {
			return page, hubapi.NativePublicInvalid("Queue cursor is invalid")
		}
		raw, err := s.config.SecretKeys.Open(envelope, []byte(cursor.Scope))
		if err != nil {
			return page, hubapi.NativePublicInvalid("Queue cursor is invalid")
		}
		err = json.Unmarshal(raw, &query.After)
		clear(raw)
		if err != nil || query.After.WorkItemID == "" || query.After.Stage < 0 || query.After.Stage > 1 {
			return page, hubapi.NativePublicInvalid("Queue cursor is invalid")
		}
		if query.After.Stage == 1 {
			query.IdentifierOrder, query.Unqueued = true, true
		}
	}
	type selected struct {
		row     claimstore.QueueListRow
		item    corestore.WorkItem
		version eventstore.CollaborationVersion
	}
	var rows []claimstore.QueueListRow
	fallbackPending := false
	selectedRows := map[tracker.NativeWorkItemID]selected{}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return page, hubapi.NativeWorkItemStorageUnavailable()
	}
	err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil || project.Profile != "github_compatible" {
			return corestore.ErrNotFound
		}
		rows, err = r.QueueListPage(ctx, query)
		if err != nil {
			return err
		}
		if !query.IdentifierOrder && len(rows) <= corestore.MaxPageSize {
			remaining := corestore.MaxPageSize - len(rows)
			if remaining == 0 {
				fallbackPending = true
			} else {
				tail := query
				tail.IdentifierOrder, tail.Unqueued = true, true
				tail.After = claimstore.QueueListCursor{}
				tail.Limit = remaining + 1
				unqueued, err := r.QueueListPage(ctx, tail)
				if err != nil {
					return err
				}
				rows = append(rows, unqueued...)
			}
		}
		var candidates []claimstore.Candidate
		var matched []claimstore.QueueListRow
		for _, row := range rows[:min(corestore.MaxPageSize, len(rows))] {
			if row.Matches {
				candidates = append(candidates, claimstore.Candidate{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: row.Entry.WorkItemID, ID: row.ID, Revision: row.Revision})
				matched = append(matched, row)
			}
		}
		batch, ok := r.(tenantstore.ClaimPreviewPlanRowsReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		metadata, err := batch.ClaimPreviewPlanRows(ctx, scope.Project, candidates)
		if err != nil {
			return err
		}
		if len(metadata) != len(candidates) {
			return corestore.ErrRevisionConflict
		}
		for index, source := range metadata {
			if source.Err != nil {
				return source.Err
			}
			if source.Item.NativeID != candidates[index].WorkItemID || source.Item.Revision != candidates[index].Revision {
				return corestore.ErrRevisionConflict
			}
			if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(source.Item.ActorPrincipalID)}) {
				continue
			}
			selectedRows[source.Item.NativeID] = selected{row: matched[index], item: source.Item, version: source.Version}
		}
		return nil
	})
	if err != nil {
		return page, hubapi.ProjectRecordError(err)
	}
	more := len(rows) > corestore.MaxPageSize || fallbackPending
	budgetExhausted, lookahead, byteClipped := more, false, false
	last := query.After
	size := 8192
	for _, row := range rows[:min(corestore.MaxPageSize, len(rows))] {
		selected, found := selectedRows[row.Entry.WorkItemID]
		if !found || !row.Matches {
			last = row.Position
			continue
		}
		issue, err := s.hydrateWorkItemRecord(ctx, selected.item, selected.version)
		if err != nil {
			return page, err
		}
		if readiness != "" {
			evidence := tracker.NativeRuntimeEvidence{}
			if err := s.readRuntimeAdmissionTenant(ctx, scope, issue, &evidence, nil); err != nil {
				return page, err
			}
			ready := slices.ContainsFunc(evidence.Admission, func(value tracker.NativeRuntimeAdmission) bool { return value.Outcome == "ready" })
			if readiness == "ready" && !ready || readiness == "waiting" && ready {
				last = row.Position
				continue
			}
		}
		issue = hubrecords.RelationshipOmittedIssue(issue)
		issue.Change, issue.ListChange, issue.PullRequest = nil, nil, nil
		issue.Queue, issue.Workpad = nil, nil
		for _, field := range []string{"change", "list_change", "pull_request"} {
			if !slices.Contains(issue.OmittedFields, field) {
				issue.OmittedFields = append(issue.OmittedFields, field)
			}
		}
		if len(page.Items) == limit {
			more, lookahead = true, true
			break
		}
		if row.Entry.Scope != "" {
			issue.Queue = &tracker.QueueSummary{Scope: row.Entry.Scope, State: row.Entry.State, Rank: row.Entry.Rank}
			if row.Entry.Priority != nil {
				priority := int(*row.Entry.Priority)
				issue.Queue.PriorityRank = &priority
			}
		}
		encoded, err := json.Marshal(issue)
		if err != nil {
			return page, err
		}
		if size+len(encoded) > operatortool.MaxResultBytes {
			if len(page.Items) == 0 {
				return page, hubapi.NativePublicInvalid("A single issue exceeds the page budget; use work_bulk_export")
			}
			more, byteClipped = true, true
			break
		}
		size += len(encoded)
		page.Items = append(page.Items, s.nativeIssueResponse(scope.ReporterIssue(issue)))
		last = row.Position
	}
	if len(page.Items) > 0 {
		var current []tenantstore.ClaimPreviewPlanRow
		err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			project, err := r.Project(ctx, scope.Project)
			if err != nil {
				return err
			}
			if project.DeletedAt != nil || project.Profile != "github_compatible" {
				return corestore.ErrNotFound
			}
			batch, ok := r.(tenantstore.ClaimPreviewPlanRowsReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			candidates := make([]claimstore.Candidate, 0, len(page.Items))
			for _, issue := range page.Items {
				selected := selectedRows[issue.WorkItemID]
				candidates = append(candidates, claimstore.Candidate{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, ID: selected.item.ID, Revision: selected.item.Revision})
			}
			current, err = batch.ClaimPreviewPlanRows(ctx, scope.Project, candidates)
			return err
		})
		if err != nil {
			return page, hubapi.ProjectRecordError(err)
		}
		if len(current) != len(page.Items) {
			return page, corestore.ErrRevisionConflict
		}
		kept := page.Items[:0]
		for index, issue := range page.Items {
			row := current[index]
			if row.Item.NativeID == "" && errors.Is(row.Err, corestore.ErrNotFound) || row.Item.NativeID == issue.WorkItemID && (row.Item.Revision != int64(issue.Revision) || row.Item.Archived || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.Item.ActorPrincipalID)})) {
				page.Partial = true
				continue
			}
			if row.Err != nil {
				return page, row.Err
			}
			if row.Item.NativeID != issue.WorkItemID || row.Version.PayloadObjectKey != selectedRows[issue.WorkItemID].version.PayloadObjectKey || row.Version.PayloadSHA256 != selectedRows[issue.WorkItemID].version.PayloadSHA256 {
				return page, corestore.ErrRevisionConflict
			}
			kept = append(kept, issue)
		}
		page.Items = kept
	}
	if more {
		if fallbackPending && !lookahead && !byteClipped {
			last.Stage, last.Number = 1, 0
		}
		page.Partial = page.Partial || budgetExhausted && !lookahead || byteClipped
		if s.config.SecretKeys == nil {
			page.Partial = true
			page.ContinuationUnavailable = true
			return page, nil
		}
		raw, err := json.Marshal(last)
		if err != nil {
			return page, err
		}
		envelope, err := s.config.SecretKeys.Seal(raw, []byte(cursor.Scope))
		clear(raw)
		if err != nil {
			return page, err
		}
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return page, err
		}
		cursor.After = string(encoded)
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return page, err
		}
		if len(page.NextCursor) > 4096 {
			return page, hubapi.NativePublicInvalid("Queue cursor exceeds its page bound")
		}
	}
	return page, nil
}
