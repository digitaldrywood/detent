package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func boardRecordSelection(scope nativeScope, params url.Values) (viewstore.BoardSelection, error) {
	includeWorkspace, err := parseNativeIssueIncludes(params.Get("include"))
	if err != nil {
		return viewstore.BoardSelection{}, err
	}
	selection := viewstore.BoardSelection{ProjectID: scope.project, Reporter: scope.credential.HostedRole == "reporter", ReporterPrincipalID: scope.actor().PrincipalID, States: params["state"], Types: params["type"], Verification: params["verification"], Labels: params["label"], Assignees: params["assignee"], ClosedOrder: params.Get("sort") == "closed", ExcludeWorkspace: !includeWorkspace, Fingerprint: strings.TrimSpace(params.Get("fingerprint"))}
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
		return selection, nativePublicInvalid("archived must be true, false or all")
	}
	switch params.Get("open") {
	case "":
	case "true":
		selection.Terminal = new(false)
	case "false":
		selection.Terminal = new(true)
	default:
		return selection, nativePublicInvalid("open must be true or false")
	}
	for _, value := range params["priority"] {
		if value == "" {
			continue
		}
		priority, err := strconv.Atoi(value)
		if err != nil || priority < 0 || priority > 3 {
			return selection, nativePublicInvalid("priority must be between zero and three")
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
				return selection, nativePublicInvalid("Completion window bounds must be RFC3339 timestamps")
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
				return selection, nativePublicInvalid("Date filters must be RFC3339 timestamps")
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

func (s *Service) readBoardRecords(ctx context.Context, scope nativeScope, params url.Values) (tracker.NativeIssuePage, error) {
	return s.readBoardRecordsFor(ctx, scope, params, false)
}

func (s *Service) readBoardRecordsFor(ctx context.Context, scope nativeScope, params url.Values, legacy bool) (tracker.NativeIssuePage, error) {
	if err := validateNativeIssueQuery(params); err != nil {
		return tracker.NativeIssuePage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var page tracker.NativeIssuePage
	read := func(ctx context.Context) error {
		var err error
		page, err = s.readBoardRecordSnapshot(ctx, scope, params, legacy)
		return err
	}
	if repository, ok := s.config.TenantRepository.(interface {
		ReadBoardRequest(context.Context, tracker.OrganizationID, string, func(context.Context) error) error
	}); ok {
		err := repository.ReadBoardRequest(ctx, scope.organization, scope.credential.ID, read)
		return page, err
	}
	if repository, ok := s.config.TenantRepository.(tenantstore.RequestReader); ok {
		err := repository.ReadRequest(ctx, scope.organization, read)
		return page, err
	}
	err := read(ctx)
	return page, err
}

func (s *Service) readBoardRecordSnapshot(ctx context.Context, scope nativeScope, params url.Values, legacy bool) (tracker.NativeIssuePage, error) {
	page := tracker.NativeIssuePage{Page: tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}}}
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return page, workItemStorageUnavailable()
	}
	if _, err := nativeIssuePickerRequested(params); err != nil {
		return page, err
	}
	includeBoard := slices.ContainsFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return strings.TrimSpace(name) == "board" })
	includeWork := slices.ContainsFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return strings.TrimSpace(name) == "work" })
	includeSummary := slices.ContainsFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return strings.TrimSpace(name) == "summary" })
	if includeSummary && includeWork {
		return page, nativePublicInvalid("summary cannot include work")
	}
	if includeBoard {
		page.Cards = []tracker.NativeBoardCard{}
		if params.Get("include") != "board" {
			return page, nativePublicInvalid("board cannot include other projections")
		}
		if params.Has("created_after") || params.Has("created_before") || params.Has("updated_after") {
			return page, nativePublicInvalid("Date filters require an issue list projection")
		}
	}
	if sort := params.Get("sort"); sort != "" && sort != "closed" {
		return page, nativePublicInvalid("sort supports closed")
	}
	selection, err := boardRecordSelection(scope, params)
	if err != nil {
		return page, err
	}
	selection.SearchText = strings.TrimSpace(params.Get("q"))
	if len(selection.SearchText) > searchstore.MaxQueryTextBytes {
		return page, nativePublicInvalid("Search query is too long")
	}
	selectedWindow, err := nativeShippedWindow(params.Get("completed_window"))
	if err != nil {
		return page, err
	}
	path, maximum := "/api/v2/organizations/"+url.PathEscape(string(scope.organization))+"/projects/"+url.PathEscape(string(scope.project))+"/work-items", maxAPIPageLimit
	if includeBoard {
		path, maximum = "/board/"+string(scope.project), 2000
	}
	cursorParams := params
	if !includeBoard {
		cursorParams = url.Values{}
		for name, values := range params {
			cursorParams[name] = values
		}
		includes := slices.DeleteFunc(strings.Split(params.Get("include"), ","), func(name string) bool { return slices.Contains([]string{"summary", "work"}, strings.TrimSpace(name)) })
		cursorParams.Del("include")
		if len(includes) > 0 {
			cursorParams.Set("include", strings.Join(includes, ","))
		}
	}
	limit, cursor, key, err := s.readNativePageLimit(ctx, scope, path, cursorParams, maximum, defaultAPIPageLimit)
	if err != nil {
		return page, err
	}
	if !legacy {
		limit = min(limit, 200)
	}
	selection.Summary = legacy && selection.SupportsSummary()
	selection.Limit = limit + 1
	selection.Compact = includeSummary || includeWork
	if position := cursor.Issues; position != nil {
		selection.After = &viewstore.BoardPosition{Lane: position.Lane, State: position.State, Priority: position.Priority, ActivityMissing: position.ActivityMissing != 0, Identifier: position.Identifier}
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
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		project, err := reader.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil || project.Profile != "native" {
			return corestore.ErrNotFound
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
			return workItemStorageUnavailable()
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
		if legacy && snapshot.Pending && err == nil {
			return searchstore.ErrIndexPending
		}
		if err != nil {
			return err
		}
		return s.authorizeBoardModel(ctx, reader, scope, &snapshot)
	})
	if err != nil {
		return page, projectRecordError(err)
	}
	if selection.AfterSequence != nil && *selection.AfterSequence != snapshot.Sequence {
		return page, nativePublicInvalid("Work list order changed; restart the query")
	}
	page.ProjectionPending, page.ProjectionOldestPendingAt = snapshot.Pending, snapshot.OldestPendingAt
	page.ChangeSequence = snapshot.Sequence
	if includeBoard {
		page.Sequence = snapshot.Sequence
	}
	page.Total = -1
	if (includeBoard || includeWork) && (!legacy || selection.Summary) {
		page.Work = &tracker.NativeWorkSummary{Items: []tracker.NativeIssue{}, Lanes: []tracker.NativeWorkLane{}, AsOf: s.config.now()}
	}
	if selection.Summary {
		page.Total = 0
		windows := []tracker.NativeShippedWindow{{Window: "1h"}, {Window: "24h"}, {Window: "7d"}}
		for _, summary := range snapshot.Summaries {
			if summary.Kind == "lane" {
				page.Work.Lanes = append(page.Work.Lanes, tracker.NativeWorkLane{State: summary.State, Total: int(summary.Total), Running: int(summary.Running)})
				if len(selection.States) > 0 && slices.Contains(selection.States, summary.State) {
					page.Total += int(summary.Total)
				}
			}
			if summary.Kind == "total" {
				windows[0].Shipped, windows[0].Cancelled = int(summary.Shipped1h), int(summary.Cancelled1h)
				windows[1].Shipped, windows[1].Cancelled = int(summary.Shipped24h), int(summary.Cancelled24h)
				windows[2].Shipped, windows[2].Cancelled = int(summary.Shipped7d), int(summary.Cancelled7d)
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
	records := snapshot.Records
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	returned := make([]viewstore.BoardRecord, 0, len(records))
	used := 0
	for _, row := range records {
		if selection.Compact && used+row.ListBytes > operatortool.WorkListPageBytes {
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
		used += row.ListBytes
		page.Items = append(page.Items, card.Issue)
		returned = append(returned, row)
		if includeBoard {
			page.Cards = append(page.Cards, card)
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

func boardModelCard(scope nativeScope, row viewstore.BoardRecord) (tracker.NativeBoardCard, error) {
	if row.PageIssue == nil {
		return tracker.NativeBoardCard{}, viewstore.ErrInvalidView
	}
	issue := *row.PageIssue
	if scope.credential.HostedRole == "reporter" {
		facts := row.ReporterRelations
		if facts == nil {
			return tracker.NativeBoardCard{}, viewstore.ErrInvalidView
		}
		issue.ReporterStatus = facts.ReporterStatus
		issue.Dependencies, issue.Blockers, issue.Dependents, issue.Related = facts.Dependencies, facts.Blockers, facts.Dependents, facts.Related
		issue.DependencyReady, issue.BlockedReason, issue.UnblocksReason = facts.DependencyReady, facts.BlockedReason, facts.UnblocksReason
	}
	card := tracker.NativeBoardCard{Issue: issue, StatusLine: row.Display.StatusLine, Attempt: row.Display.Attempt, Execution: row.Display.Execution, Change: row.Display.Change}
	if !scope.reporterOwns(tracker.Actor{PrincipalID: row.Display.ChangePrincipalID}) {
		card.Change = nil
	}
	return card, nil
}

func laneSelected(state string, selection viewstore.BoardSelection, states []policy.State) bool {
	if len(selection.States) > 0 && !slices.Contains(selection.States, state) {
		return false
	}
	if selection.Terminal == nil {
		return true
	}
	for _, candidate := range states {
		if candidate.Name == state {
			return candidate.Terminal == *selection.Terminal
		}
	}
	return false
}

func (s *Service) applyIssueSearchHit(ctx context.Context, scope nativeScope, params url.Values, item tracker.NativeWorkItemID, issue *tracker.NativeIssue) error {
	text := strings.TrimSpace(params.Get("q"))
	if len(text) > searchstore.MaxQueryTextBytes {
		return nativePublicInvalid("Search query is too long")
	}
	query := searchstore.Query{Kind: "work", ProjectID: scope.project, WorkItemID: item, Text: text, Archived: params.Get("archived"), Limit: 1}
	if scope.credential.HostedRole == "reporter" {
		principal := scope.actor().PrincipalID
		query.Reporter = &principal
	}
	matches, err := searchstore.NewContent(s.runDetailStore()).Find(ctx, tenantSearchRepository{boardSearchRepository{Repository: s.config.TenantRepository, service: s, scope: scope}}, scope.organization, query)
	if err != nil {
		return err
	}
	if len(matches.Items) != 1 || matches.Items[0].Document.WorkItemID != item {
		return searchstore.ErrIndexPending
	}
	issue.Snippet, issue.SearchScore = matches.Items[0].Snippet, matches.Items[0].Score
	return nil
}

func (s *Service) boardAccessVersion(ctx context.Context, r tenantstore.Reader, scope nativeScope) (string, error) {
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
	raw, err := json.Marshal([]any{scope.organization, authority.CredentialID, authority.UserID, authority.Role, memberRole, credential.Scope, credential.ProjectAccess, credential.KeyScope, revisions, scope.credential.EntryKey})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

type boardSearchRepository struct {
	tenantstore.Repository
	service *Service
	scope   nativeScope
}

func (r boardSearchRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	if organization != r.scope.organization {
		return corestore.ErrNotFound
	}
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		if err := r.service.authorizeAccessScope(ctx, reader, r.scope, false); err != nil {
			return err
		}
		return read(reader)
	})
}
