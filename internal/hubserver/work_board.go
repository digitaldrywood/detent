package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/boardview"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const workBoardClosedPage = 20

type workBoardLane struct {
	State      string `json:"state"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type workBoardView struct {
	Projects     []boardview.Project       `json:"projects"`
	Cards        []tracker.NativeBoardCard `json:"cards"`
	Lanes        []workBoardLane           `json:"lanes"`
	GrantVersion string                    `json:"grant_version"`
}

type workBoardRequest struct {
	state    savedWorkState
	projects []tracker.ProjectID
	items    []tracker.NativeWorkItemID
	lane     string
	cursor   string
	params   url.Values
}

func parseWorkBoardRequest(params url.Values) (workBoardRequest, error) {
	request := workBoardRequest{params: params}
	invalid := hubapi.NativePublicInvalid("Invalid board view")
	for key, values := range params {
		switch key {
		case "project", "item":
		case "state", "lane", "cursor":
			if len(values) != 1 {
				return request, invalid
			}
		default:
			return request, invalid
		}
	}
	if len(params["state"]) != 1 || len(params.Get("state")) > maxSavedWorkViewBytes {
		return request, invalid
	}
	decoder := json.NewDecoder(strings.NewReader(params.Get("state")))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request.state); err != nil {
		return request, invalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return request, invalid
	}
	ids := params["project"]
	if len(ids) == 0 || len(ids) > viewstore.BoardViewMaxProjects || len(params["item"]) > viewstore.BoardViewMaxItems {
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
	if !validSavedWorkView(savedWorkView{Name: "Work", Scope: "personal", ProjectIDs: ids[:1], State: request.state}) || request.state.View != "board" || strings.TrimSpace(request.state.Q) != "" || request.state.CompletedFrom != "" || request.state.CompletedTo != "" {
		return request, invalid
	}
	for _, id := range params["item"] {
		if id == "" || len(id) > 512 {
			return request, invalid
		}
		request.items = append(request.items, tracker.NativeWorkItemID(id))
	}
	request.lane, request.cursor = params.Get("lane"), params.Get("cursor")
	if request.cursor != "" && request.lane == "" || request.lane != "" && len(request.items) > 0 {
		return request, invalid
	}
	return request, nil
}

func (s *Service) workBoard(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	credential, _, err := s.appCredential(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	org := s.appOrganizationID()
	if c.Param("organization") != org || credential.Scope == hubapi.APIScopeWorker {
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
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(org), Credential: credential}
	for _, id := range request.projects {
		if key := credential.EntryKey; key != nil && !key.Allows(string(id)) {
			return s.nativeAPIError(c, hubapi.NativeNotFound())
		}
	}
	view, err := s.readWorkBoard(ctx, scope, request)
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.JSON(http.StatusOK, view)
}

func (s *Service) readWorkBoard(ctx context.Context, scope hubscope.NativeScope, request workBoardRequest) (workBoardView, error) {
	window, err := nativeShippedWindow(request.state.CompletedWindow)
	if err != nil {
		return workBoardView{}, err
	}
	selection := viewstore.BoardViewSelection{Items: request.items, Archived: request.state.Archived, CredentialID: scope.Credential.ID, PrincipalID: scope.Credential.HostedPrincipal,
		ClosedPage: workBoardClosedPage, ClosedLane: request.lane}
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
		if position.State != request.lane {
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
	for attempt := range 2 {
		if entry, err = s.boardAllowedProjects(ctx, scope, request.projects); err != nil {
			return workBoardView{}, err
		}
		selection.Allowed, selection.Projects = entry.allowed, nil
		for _, id := range request.projects {
			if !slices.Contains(entry.allowed, id) {
				return workBoardView{}, hubapi.NativeNotFound()
			}
			selection.Projects = append(selection.Projects, id)
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
			break
		}
		s.boardAccess.drop(boardAccessKey(scope, request.projects))
		if attempt == 1 {
			return workBoardView{}, hubapi.NativeWorkItemStorageUnavailable()
		}
	}
	assembled, positions, err := boardview.Assemble(scope, len(request.items) > 0, rows, window, s.config.Clock(), workBoardClosedPage)
	if err != nil {
		return workBoardView{}, err
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
	for _, position := range positions {
		lane := workBoardLane{State: position.State}
		if position.More {
			next := cursor
			next.Issues = &nativeIssuePageCursor{State: position.State, Identifier: position.Identifier}
			if position.ClosedAt != nil {
				next.Issues.Activity = position.ClosedAt.UTC().Format(time.RFC3339Nano)
			}
			if lane.NextCursor, err = encodeNativeCursor(next, key); err != nil {
				return workBoardView{}, err
			}
		}
		view.Lanes = append(view.Lanes, lane)
	}
	return view, nil
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
