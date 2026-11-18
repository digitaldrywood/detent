package hubserver

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readTestWorkBoard(t *testing.T, ctx context.Context, service *Service, scope hubscope.NativeScope, state savedWorkState, projects ...tracker.ProjectID) workBoardView {
	t.Helper()
	if len(projects) == 0 {
		projects = []tracker.ProjectID{scope.Project}
	}
	request, err := parseWorkBoardRequest(boardViewParams(t, state, projects))
	if err != nil {
		t.Fatal(err)
	}
	scope.Project = ""
	view, err := service.readWorkBoard(ctx, scope, request)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func testBoardWork(view workBoardView) *tracker.NativeWorkSummary {
	if len(view.Projects) == 0 {
		return nil
	}
	return view.Projects[0].Work
}

func boardViewParams(t *testing.T, state savedWorkState, projects []tracker.ProjectID, items ...tracker.NativeWorkItemID) url.Values {
	t.Helper()
	if state.Tab == "" {
		state.Tab = "all"
	}
	if state.Sort == "" {
		state.Sort = "default"
	}
	if state.CompletedWindow == "" {
		state.CompletedWindow = "24h"
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	params := url.Values{"state": {string(raw)}}
	for _, id := range projects {
		params.Add("project", string(id))
	}
	for _, id := range items {
		params.Add("item", string(id))
	}
	return params
}
