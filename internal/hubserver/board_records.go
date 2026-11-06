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
	page := tracker.NativeIssuePage{Page: tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}}}
	ctx, cancel := context.WithTimeout(s.runDetailContext(ctx), hubReadTimeout)
	defer cancel()
	ctx = s.withRequestDetails(ctx)
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return page, workItemStorageUnavailable()
	}
	if err := validateNativeIssueQuery(params); err != nil {
		return page, err
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
	finishCursor := timeSpan(ctx, "board_cursor")
	limit, cursor, key, err := s.readNativePageLimit(ctx, scope, path, cursorParams, maximum, defaultAPIPageLimit)
	finishCursor()
	if err != nil {
		return page, err
	}
	selection.SearchRanked = selection.SearchText != "" && !includeBoard
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
	now := s.config.now()
	starts := make([]time.Time, len(nativeShippedWindowSpans))
	for index, span := range nativeShippedWindowSpans {
		starts[index] = now.Add(-span.span)
	}
	var records []viewstore.BoardRecord
	var workRecordsForResponse []viewstore.BoardRecord
	var projectSlug string
	snapshotRepository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return page, workItemStorageUnavailable()
	}
	finishSelection := timeSpan(ctx, "board_select")
	err = snapshotRepository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		if includeBoard {
			page.GrantVersion, err = s.boardAccessVersion(ctx, r, scope)
			if err != nil {
				return err
			}
		}
		project, err := r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		projectSlug = project.Core.Slug
		sequence, err := r.BoardSequence(ctx, scope.project)
		if err != nil {
			return err
		}
		page.ChangeSequence = sequence.Sequence
		if selection.AfterSequence != nil && *selection.AfterSequence != sequence.Sequence {
			return nativePublicInvalid("Work list order changed; restart the query")
		}
		if includeBoard {
			page.Sequence = sequence.Sequence
		}
		lanes, err := r.BoardLanes(ctx, selection, now)
		if err != nil {
			return err
		}
		if includeBoard || includeWork {
			page.Work = &tracker.NativeWorkSummary{Items: []tracker.NativeIssue{}, Lanes: []tracker.NativeWorkLane{}, AsOf: now}
		}
		for _, lane := range lanes {
			if page.Work != nil && (includeBoard || laneSelected(lane.State, selection, project.States)) {
				page.Work.Lanes = append(page.Work.Lanes, tracker.NativeWorkLane{State: lane.State, Total: int(lane.Total), Running: int(lane.Running)})
			}
			if len(selection.States) != 0 && !slices.Contains(selection.States, lane.State) {
				continue
			}
			if selection.Terminal != nil {
				terminal := false
				for _, state := range project.States {
					if state.Name == lane.State {
						terminal = state.Terminal
					}
				}
				if terminal != *selection.Terminal {
					continue
				}
			}
			page.Total += int(lane.Total)
		}
		if page.Work != nil {
			counts, err := r.BoardClosedWindows(ctx, selection, starts, now)
			if err != nil {
				return err
			}
			windows := make([]tracker.NativeShippedWindow, len(starts))
			for index, span := range nativeShippedWindowSpans {
				windows[index].Window = span.name
			}
			for _, count := range counts {
				switch count.State {
				case nativeShippedState:
					windows[count.Window].Shipped = int(count.Total)
				case nativeCancelledState:
					windows[count.Window].Cancelled = int(count.Total)
				}
			}
			applyNativeShippedWindows(page.Work, windows, selectedWindow)
		}
		for len(records) < limit+1 {
			selection.Limit = min(corestore.MaxPageSize, limit+1-len(records))
			batch, err := r.BoardPage(ctx, selection)
			if err != nil {
				return err
			}
			records = append(records, batch...)
			if len(batch) < selection.Limit {
				break
			}
			last := batch[len(batch)-1]
			selection.After = &last.Position
			if selection.ClosedOrder {
				selection.After.Activity = last.ClosedAt
			}
		}
		if includeWork {
			workSelection := selection
			workSelection.After = nil
			workSelection.Terminal = new(false)
			workSelection.WorkOrder = true
			workSelection.Limit = min(corestore.MaxPageSize, limit+1)
			workRecords, err := r.BoardPage(ctx, workSelection)
			if err != nil {
				return err
			}
			page.Work.Truncated = len(workRecords) > limit
			if page.Work.Truncated {
				workRecords = workRecords[:limit]
			}
			workRecordsForResponse = append(workRecordsForResponse, workRecords...)
		}
		return nil
	})
	finishSelection()
	if err != nil {
		return page, projectRecordError(err)
	}
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	finishSlug := timeSpan(ctx, "board_slug")
	organizationSlug, err := s.readableOrganizationSlug(ctx)
	finishSlug()
	if err != nil {
		return page, err
	}
	finishHydration := timeSpan(ctx, "board_hydrate")
	defer finishHydration()
	prepared := make(map[tracker.NativeWorkItemID]boardPreparedRecord, len(records)+len(workRecordsForResponse))
	preparation := s.prepareBoardPage(ctx, scope, records, now, includeBoard, prepared)
	defer preparation.close()
	returnedRecords := make([]viewstore.BoardRecord, 0, min(len(records), limit))
	for index, record := range records {
		card, err := preparation.card(ctx, index)
		if err != nil {
			return page, err
		}
		card.Issue.WebURL = issueBrowserPath(organizationSlug, projectSlug, card.Issue.Number)
		if selection.SearchText != "" {
			if err := s.applyIssueSearchHit(ctx, scope, params, record.WorkItemID, &card.Issue); err != nil {
				return page, err
			}
		}
		card.Issue = s.nativeIssueResponse(scope.reporterIssue(card.Issue))
		if includeSummary || includeWork {
			card.Issue = operatortool.NativeListIssue(card.Issue)
			candidate := tracker.Page[tracker.NativeIssue]{Items: append(page.Items, card.Issue)}
			raw, err := json.Marshal(operatortool.NativeItemPage(string(scope.project), candidate))
			if err != nil {
				return page, err
			}
			if len(raw) > operatortool.WorkListPageBytes {
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
		if includeBoard {
			card.StatusLine = nativeBoardStatusLine(card)
			page.Cards = append(page.Cards, card)
		}
		page.Items = append(page.Items, card.Issue)
		returnedRecords = append(returnedRecords, record)
	}
	preparation.close()
	if includeWork {
		workPreparation := s.prepareBoardPage(ctx, scope, workRecordsForResponse, now, false, prepared)
		defer workPreparation.close()
		for index, record := range workRecordsForResponse {
			card, err := workPreparation.card(ctx, index)
			if err != nil {
				return page, err
			}
			if selection.SearchText != "" {
				if err := s.applyIssueSearchHit(ctx, scope, params, record.WorkItemID, &card.Issue); err != nil {
					return page, err
				}
			}
			card.Issue = operatortool.NativeListIssue(s.nativeIssueResponse(scope.reporterIssue(card.Issue)))
			candidate := tracker.Page[tracker.NativeIssue]{Items: append(page.Work.Items, card.Issue)}
			raw, err := json.Marshal(operatortool.NativeItemPage(string(scope.project), candidate))
			if err != nil {
				return page, err
			}
			if len(raw) > operatortool.WorkListPageBytes {
				if len(page.Work.Items) > 0 {
					page.Work.Truncated = true
					break
				}
				card.Issue, err = operatortool.BoundNativeListIssue(card.Issue)
				if err != nil {
					return page, err
				}
			}
			page.Work.Items = append(page.Work.Items, card.Issue)
		}
	}
	if hasMore {
		if len(returnedRecords) == 0 {
			return page, operatortool.ErrResultTooLarge
		}
		cursor.Issues = boardRecordCursor(returnedRecords[len(returnedRecords)-1], selection.ClosedOrder, page.ChangeSequence)
		page.NextCursor, err = encodeNativeCursor(cursor, key)
	}
	return page, err
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
