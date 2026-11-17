package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/boardview"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	workBoardClosedPage   = 20
	workBoardMaxLanes     = 64
	workBoardCompactBytes = operatortool.MaxResultBytes - 16<<10
)

type workBoardLane struct {
	State      string `json:"state"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type workBoardView struct {
	Projects     []boardview.Project       `json:"projects"`
	Cards        []tracker.NativeBoardCard `json:"cards"`
	Lanes        []workBoardLane           `json:"lanes"`
	GrantVersion string                    `json:"grant_version"`
	compact      *boardview.CompactBoard
}

type workBoardRequest struct {
	state    savedWorkState
	projects []tracker.ProjectID
	items    []tracker.NativeWorkItemID
	lanes    boardview.Lanes
	cursor   string
	budget   int
	params   url.Values
}

func defaultWorkBoardState() savedWorkState {
	return savedWorkState{View: "board", Tab: "all", Sort: "default", CompletedWindow: "24h"}
}

func workBoardLaneNames(values []string) ([]string, bool) {
	if len(values) > workBoardMaxLanes {
		return nil, false
	}
	names := []string{}
	for _, name := range values {
		if name == "" || len(name) > 256 || slices.Contains(names, name) {
			return nil, false
		}
		names = append(names, name)
	}
	return names, true
}

func parseWorkBoardRequest(params url.Values) (workBoardRequest, error) {
	request := workBoardRequest{params: params, state: defaultWorkBoardState()}
	invalid := hubapi.NativePublicInvalid("Invalid board view")
	for key, values := range params {
		switch key {
		case "project", "item", "lane", "exclude_lane":
		case "state", "lanes", "cursor", "format":
			if len(values) != 1 {
				return request, invalid
			}
		default:
			return request, invalid
		}
	}
	if raw, found := params["state"]; found {
		if len(raw[0]) > maxSavedWorkViewBytes {
			return request, invalid
		}
		request.state = savedWorkState{}
		decoder := json.NewDecoder(strings.NewReader(raw[0]))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request.state); err != nil {
			return request, invalid
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return request, invalid
		}
	}
	ids := params["project"]
	if len(ids) > viewstore.BoardViewMaxProjects || len(params["item"]) > viewstore.BoardViewMaxItems {
		return request, invalid
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || len(id) > 512 || seen[id] {
			return request, invalid
		}
		seen[id] = true
		request.projects = append(request.projects, tracker.ProjectID(id))
	}
	if !validSavedWorkView(savedWorkView{Name: "Work", Scope: "personal", ProjectIDs: []string{"board"}, State: request.state}) || request.state.View != "board" || strings.TrimSpace(request.state.Q) != "" || request.state.CompletedFrom != "" || request.state.CompletedTo != "" {
		return request, invalid
	}
	for _, id := range params["item"] {
		if id == "" || len(id) > 512 {
			return request, invalid
		}
		request.items = append(request.items, tracker.NativeWorkItemID(id))
	}
	var ok bool
	if request.lanes.Include, ok = workBoardLaneNames(params["lane"]); !ok {
		return request, hubapi.NativePublicInvalid("lane and exclude_lane accept at most 64 distinct names of at most 256 bytes")
	}
	if request.lanes.Exclude, ok = workBoardLaneNames(params["exclude_lane"]); !ok {
		return request, hubapi.NativePublicInvalid("lane and exclude_lane accept at most 64 distinct names of at most 256 bytes")
	}
	request.lanes.Preset = params.Get("lanes")
	if request.lanes.Preset == "" {
		request.lanes.Preset = viewstore.LanesOpen
		if len(request.lanes.Include) > 0 {
			request.lanes.Preset = viewstore.LanesAll
		}
	}
	if !viewstore.ValidLanePreset(request.lanes.Preset) {
		return request, hubapi.NativePublicInvalid("lanes must be open, active or all")
	}
	switch params.Get("format") {
	case "", "full":
	case "compact":
		request.budget = workBoardCompactBytes
	default:
		return request, hubapi.NativePublicInvalid("format must be full or compact")
	}
	request.cursor = params.Get("cursor")
	if request.cursor != "" && len(request.lanes.Include) != 1 {
		return request, hubapi.NativePublicInvalid("cursor requires exactly one lane")
	}
	if len(request.items) > 0 && (len(request.projects) == 0 || request.cursor != "" || len(request.lanes.Include) > 0 || len(request.lanes.Exclude) > 0 || params.Has("lanes")) {
		return request, invalid
	}
	return request, nil
}

func (s *Service) workBoard(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	request, err := parseWorkBoardRequest(params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	view, err := s.readWorkBoardFor(ctx, credential, c.Param("organization"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if view.compact != nil {
		return boardview.Respond(c.Response(), c.Request(), http.StatusOK, view.compact)
	}
	return boardview.Respond(c.Response(), c.Request(), http.StatusOK, view)
}

func (s *Service) readWorkBoardFor(ctx context.Context, credential hubapi.APICredential, organization string, request workBoardRequest) (workBoardView, error) {
	if organization != s.appOrganizationID() || credential.Scope == hubapi.APIScopeWorker || credential.Runner.RunnerID != "" {
		return workBoardView{}, hubapi.NativeNotFound()
	}
	if s.config.TenantRepository == nil {
		return workBoardView{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	view, err := s.readWorkBoard(ctx, hubscope.NativeScope{Organization: tracker.OrganizationID(organization), Credential: credential}, request)
	if err != nil {
		return workBoardView{}, hubapi.ProjectRecordError(err)
	}
	return view, nil
}

func (s *Service) readWorkBoard(ctx context.Context, scope hubscope.NativeScope, request workBoardRequest) (workBoardView, error) {
	window, err := nativeShippedWindow(request.state.CompletedWindow)
	if err != nil {
		return workBoardView{}, err
	}
	selection := viewstore.BoardViewSelection{Items: request.items, Archived: request.state.Archived, CredentialID: scope.Credential.ID, PrincipalID: scope.Credential.HostedPrincipal,
		ClosedPage: workBoardClosedPage, LanePreset: request.lanes.Preset, IncludeLanes: request.lanes.Include, ExcludeLanes: request.lanes.Exclude}
	if scope.Credential.Hosted != nil {
		selection.UserID = scope.Credential.Hosted.Subject
	}
	if scope.Credential.HostedRole == "reporter" {
		if selection.Reporter = scope.Actor().PrincipalID; selection.Reporter == "" {
			return workBoardView{}, hubapi.NativeNotFound()
		}
	}
	cursorParams := url.Values{"project": request.params["project"], "state": request.params["state"]}
	if request.cursor != "" {
		cursorParams.Set("cursor", request.cursor)
	}
	_, cursor, key, err := s.readNativePageLimit(ctx, scope, "/work-board", cursorParams, viewstore.BoardViewMaxPage, workBoardClosedPage)
	if err != nil {
		return workBoardView{}, err
	}
	if position := cursor.Issues; position != nil {
		if position.State != request.lanes.Include[0] {
			return workBoardView{}, corestore.ErrInvalidCursor
		}
		after := &viewstore.BoardClosedPosition{Identifier: position.Identifier}
		if position.Activity != "" {
			at, err := time.Parse(time.RFC3339Nano, position.Activity)
			if err != nil {
				return workBoardView{}, corestore.ErrInvalidCursor
			}
			after.ClosedAt = &at
		}
		selection.ClosedAfter = after
	}
	var rows viewstore.BoardViewRows
	var entry boardAccessEntry
	var unavailable []tracker.ProjectID
	for attempt := range 2 {
		if entry, err = s.boardAllowedProjects(ctx, scope); err != nil {
			return workBoardView{}, err
		}
		selection.Allowed, selection.Projects, unavailable = entry.allowed, nil, nil
		if len(request.projects) == 0 {
			selection.Projects = entry.allowed
		}
		for _, id := range request.projects {
			if slices.Contains(entry.allowed, id) {
				selection.Projects = append(selection.Projects, id)
			} else {
				unavailable = append(unavailable, id)
			}
		}
		if len(selection.Projects) > viewstore.BoardViewMaxProjects {
			return workBoardView{}, hubapi.NativePublicInvalid("Name at most 500 projects")
		}
		err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			model, ok := reader.(interface {
				BoardView(context.Context, viewstore.BoardViewSelection) (viewstore.BoardViewRows, error)
			})
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			rows, err = model.BoardView(ctx, selection)
			return err
		})
		if err != nil {
			return workBoardView{}, err
		}
		if boardViewRevisions(rows) == entry.revisions {
			if len(selection.Projects) == 0 {
				return workBoardView{}, hubapi.NativeNotFound()
			}
			break
		}
		s.boardAccess.drop(boardAccessKey(scope))
		if attempt == 1 {
			return workBoardView{}, hubapi.NativeWorkItemStorageUnavailable()
		}
	}
	assembled, positions, err := boardview.Assemble(scope, len(request.items) > 0, rows, window, s.config.Clock(), workBoardClosedPage)
	if err != nil {
		return workBoardView{}, err
	}
	if len(request.items) == 0 {
		if name := request.lanes.Unknown(assembled.Projects); name != "" {
			return workBoardView{}, hubapi.NativePublicInvalid("Unknown lane " + strconv.Quote(name))
		}
	}
	view := workBoardView{Projects: assembled.Projects, Cards: assembled.Cards, Lanes: []workBoardLane{}}
	for i := range view.Cards {
		view.Cards[i].Issue = boardIssueCollections(view.Cards[i].Issue)
		view.Cards[i].StatusLine = nativeBoardStatusLine(view.Cards[i])
	}
	raw, err := json.Marshal([]any{scope.Organization, scope.Credential.ID, selection.UserID, scope.Credential.HostedRole, entry.revisions, scope.Credential.EntryKey})
	if err != nil {
		return workBoardView{}, err
	}
	digest := sha256.Sum256(raw)
	view.GrantVersion = hex.EncodeToString(digest[:])
	if request.budget == 0 {
		view.Lanes, err = workBoardCursors(positions, cursor, key)
		return view, err
	}
	for budget := request.budget; ; {
		compact, cut, err := boardview.Compact(scope.Organization, s.config.Clock(), assembled, positions, request.lanes, unavailable, budget)
		if errors.Is(err, boardview.ErrTooLarge) {
			return workBoardView{}, hubapi.NativePublicInvalid("This board exceeds the result limit; name fewer projects or lanes")
		}
		if err != nil {
			return workBoardView{}, err
		}
		if view.Lanes, err = workBoardCursors(cut, cursor, key); err != nil {
			return workBoardView{}, err
		}
		for i, continuation := range compact.Continuations {
			for _, lane := range view.Lanes {
				if lane.State == continuation.State {
					compact.Continuations[i].NextCursor = lane.NextCursor
				}
			}
		}
		raw, err := json.Marshal(compact)
		if err != nil {
			return workBoardView{}, err
		}
		if len(raw) <= request.budget {
			view.compact = &compact
			return view, nil
		}
		budget -= len(raw) - request.budget + 512
	}
}

func workBoardCursors(positions []boardview.Cursor, cursor nativeCursor, key []byte) ([]workBoardLane, error) {
	lanes := []workBoardLane{}
	for _, position := range positions {
		lane := workBoardLane{State: position.State}
		if position.More {
			next := cursor
			next.Issues = &nativeIssuePageCursor{State: position.State, Identifier: position.Identifier}
			if position.ClosedAt != nil {
				next.Issues.Activity = position.ClosedAt.UTC().Format(time.RFC3339Nano)
			}
			var err error
			if lane.NextCursor, err = encodeNativeCursor(next, key); err != nil {
				return nil, err
			}
		}
		lanes = append(lanes, lane)
	}
	return lanes, nil
}

func boardViewRevisions(rows viewstore.BoardViewRows) [4]int64 {
	var revisions [4]int64
	for _, row := range rows.Rows {
		if row.Kind != "access" {
			continue
		}
		for i := range revisions {
			if row.Int[i] != nil {
				revisions[i] = *row.Int[i]
			}
		}
	}
	return revisions
}
