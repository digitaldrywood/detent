package templates

import (
	"html"
	"strings"
	"testing"
)

func TestAIDebugActionsRenderAtRequiredSurfaces(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	board := projectKanbanBoardView(data)
	if len(board.AllLanes) == 0 || len(board.AllLanes[0].Cards) == 0 {
		t.Fatal("board fixture has no cards")
	}
	card := board.AllLanes[0].Cards[0]
	tests := []struct {
		name      string
		html      string
		wantPath  string
		wantExtra string
	}{
		{
			name:      "card detail sheet",
			html:      renderBoardComponent(t, BoardCardSheet(data, card, false, false, KanbanConversationData{}, BoardActivityData{}, BoardSessionData{})),
			wantPath:  html.EscapeString(aiDebugIssuePath(card.ProjectID, card.Identity)),
			wantExtra: "Contains private fleet detail",
		},
		{
			name:      "project header",
			html:      renderBoardComponent(t, ProjectBoardPage(data)),
			wantPath:  html.EscapeString(aiDebugProjectPath(data.ProjectID)),
			wantExtra: "Contains private fleet detail",
		},
		{
			name:      "fleet health",
			html:      renderBoardComponent(t, HealthPageV2(data)),
			wantPath:  "/api/v1/ai-debug?scope=fleet",
			wantExtra: "Contains private fleet detail",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, want := range []string{"AI Debug", tt.wantPath, tt.wantExtra, "Do not paste into a public or shared context"} {
				if !strings.Contains(tt.html, want) {
					t.Fatalf("rendered action missing %q:\n%s", want, tt.html)
				}
			}
		})
	}
}

func TestAIDebugCardActionIsComfyOnly(t *testing.T) {
	t.Parallel()

	html := renderBoardComponent(t, boardCardView2(boardCardView{DomID: "card-2006", Project: "detent", Identity: "digitaldrywood/detent#2006", Title: "AI Debug"}))
	marker := `data-board-card-content="comfy" data-ai-debug-card-action`
	if !strings.Contains(html, marker) {
		t.Fatalf("card action missing comfy marker:\n%s", html)
	}
	for _, forbidden := range []string{`data-board-card-content="compact" data-ai-debug-card-action`, `data-board-card-content="cozy" data-ai-debug-card-action`} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("card action rendered in non-comfy density %q:\n%s", forbidden, html)
		}
	}
}

func TestAIDebugScriptFetchesPromptOnlyOnClick(t *testing.T) {
	t.Parallel()

	html := renderBoardComponent(t, aiDebugScript())
	for _, want := range []string{"[data-ai-debug-url]", "event.stopPropagation()", "fetch(url", `"HX-Request": "true"`, "navigator.clipboard.writeText(value).catch", "copyFallback(value)"} {
		if !strings.Contains(html, want) {
			t.Fatalf("AI Debug script missing %q:\n%s", want, html)
		}
	}
}
