package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type workBoardProject struct {
	Project          tracker.NativeProject      `json:"project"`
	Issues           []tracker.NativeIssue      `json:"issues"`
	Cards            []tracker.NativeBoardCard  `json:"cards"`
	Sequence         int64                      `json:"sequence"`
	GrantVersion     string                     `json:"grantVersion"`
	Work             *tracker.NativeWorkSummary `json:"work,omitempty"`
	NextCursor       string                     `json:"nextCursor,omitempty"`
	PageCount        int                        `json:"pageCount"`
	BacklogCursor    string                     `json:"backlogCursor,omitempty"`
	BacklogPageCount int                        `json:"backlogPageCount"`
	BacklogTotal     int                        `json:"backlogTotal"`
}

func (s *Service) workBoard(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	credential, _, err := s.appCredential(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	org := s.appOrganizationID()
	if c.Param("organization") != org {
		return s.nativeAPIError(c, nativeNotFound())
	}
	params, err := nativeReadQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if len(params) != 2 || len(params["state"]) != 1 || len(params.Get("state")) > maxSavedWorkViewBytes {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid working set"))
	}
	input := savedWorkView{Name: "Work", Scope: "personal", ProjectIDs: params["project"]}
	decoder := json.NewDecoder(strings.NewReader(params.Get("state")))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input.State); err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid display state"))
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid display state"))
	}
	if len(input.ProjectIDs) == 0 || !validSavedWorkView(input) {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid working set or display state"))
	}
	scopes := make([]nativeScope, len(input.ProjectIDs))
	for i, id := range input.ProjectIDs {
		scopes[i] = nativeScope{organization: tracker.OrganizationID(org), project: tracker.ProjectID(id), credential: credential}
		if err := s.requireHostedProject(ctx, s.database.reader, scopes[i], false); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	projects := make([]workBoardProject, 0, len(scopes))
	for _, scope := range scopes {
		project, err := s.readWorkBoardProject(ctx, scope, input.State)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		projects = append(projects, project)
	}
	return c.JSON(http.StatusOK, projects)
}

func (s *Service) readWorkBoardProject(ctx context.Context, scope nativeScope, state savedWorkState) (workBoardProject, error) {
	project, err := readNativeProject(ctx, s.database.reader, scope)
	if err != nil {
		return workBoardProject{}, fmt.Errorf("read project: %w", err)
	}
	result := workBoardProject{Project: project, Issues: []tracker.NativeIssue{}, Cards: []tracker.NativeBoardCard{}, PageCount: 1}
	params := url.Values{"include": {"board"}, "limit": {"100"}, "completed_window": {state.CompletedWindow}, "type": state.Type, "label": state.Label, "assignee": state.Assignee, "priority": state.Priority}
	if state.CompletedFrom != "" {
		params.Set("completed_from", state.CompletedFrom)
	}
	if state.CompletedTo != "" {
		params.Set("completed_to", state.CompletedTo)
	}
	if strings.TrimSpace(state.Q) != "" {
		params.Set("q", strings.TrimSpace(state.Q))
	}
	if state.CreatedBy != "" {
		params.Set("created_by", state.CreatedBy)
	}
	if state.Archived {
		params.Set("archived", "true")
	}
	var lanes []string
	if state.View == "board" {
		for _, lane := range project.States {
			if !lane.Terminal && (state.Lanes == nil || slices.Contains(state.Lanes, lane.Name)) && (len(state.State) == 0 || slices.Contains(state.State, lane.Name)) {
				lanes = append(lanes, lane.Name)
			}
		}
		params.Set("open", "true")
	} else {
		switch state.Tab {
		case "active", "backlog":
			for _, lane := range project.States {
				if state.Tab == "active" && !lane.Terminal && !strings.EqualFold(lane.Name, "Backlog") || state.Tab == "backlog" && strings.EqualFold(lane.Name, "Backlog") {
					params.Add("state", lane.Name)
				}
			}
			if state.Tab == "active" {
				params.Set("open", "true")
			}
		case "closed":
			params.Set("open", "false")
			if state.CompletedFrom != "" || state.CompletedTo != "" {
				params["state"] = state.State
			}
		}
		if state.Sort == "closed" || state.Tab == "closed" && state.Sort == "default" {
			params.Set("sort", "closed")
		}
	}
	if len(lanes) == 0 {
		lanes = []string{""}
	}
	for i, lane := range lanes {
		if state.View == "board" {
			params.Set("state", lane)
			params.Set("limit", "2000")
			if lane == "" {
				params.Set("limit", "1")
			}
			if strings.EqualFold(lane, "Backlog") {
				params.Set("limit", "200")
			}
		}
		page, err := s.readBoardIssues(ctx, scope, params)
		if err != nil {
			return result, fmt.Errorf("read board page: %w", err)
		}
		if i == 0 || page.ChangeSequence < result.Sequence {
			result.Sequence = page.ChangeSequence
		}
		result.GrantVersion = page.GrantVersion
		result.Work = page.Work
		if state.View != "board" || lane != "" {
			result.Issues = append(result.Issues, page.Items...)
			result.Cards = append(result.Cards, page.Cards...)
		}
		if state.View == "list" {
			result.NextCursor = page.NextCursor
		} else if strings.EqualFold(lane, "Backlog") {
			result.BacklogCursor = page.NextCursor
			result.BacklogPageCount = 1
			result.BacklogTotal = page.Total
		}
	}
	return result, nil
}
