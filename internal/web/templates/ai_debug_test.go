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
		name       string
		html       string
		wantPath   string
		wantDialog bool
	}{
		{
			name:     "card detail sheet",
			html:     renderBoardComponent(t, BoardCardSheet(data, card, false, false, KanbanConversationData{}, BoardActivityData{}, BoardSessionData{})),
			wantPath: html.EscapeString(aiDebugIssuePath(card.ProjectID, card.Identity)),
		},
		{
			name:       "project header",
			html:       renderBoardComponent(t, ProjectBoardPage(data)),
			wantPath:   html.EscapeString(aiDebugProjectPath(data.ProjectID)),
			wantDialog: true,
		},
		{
			name:       "fleet health",
			html:       renderBoardComponent(t, HealthPageV2(data)),
			wantPath:   "/api/v1/ai-debug?scope=fleet",
			wantDialog: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, want := range []string{"AI Debug", tt.wantPath} {
				if !strings.Contains(tt.html, want) {
					t.Fatalf("rendered action missing %q:\n%s", want, tt.html)
				}
			}
			if strings.Contains(tt.html, "data-ai-debug-privacy") {
				t.Fatalf("rendered action contains the removed inline privacy notice:\n%s", tt.html)
			}
			if got := strings.Count(tt.html, "id=\"ai-debug-privacy-dialog\""); got != boolInt(tt.wantDialog) {
				t.Fatalf("privacy dialog count = %d, want %d", got, boolInt(tt.wantDialog))
			}
		})
	}
}

func TestAIDebugCardActionIsComfyOnly(t *testing.T) {
	t.Parallel()

	html := renderBoardComponent(t, boardCardView2(boardCardView{DomID: "card-2006", Project: "detent", Identity: "digitaldrywood/detent#2006", Title: "AI Debug"}))
	marker := "data-ai-debug-card-action"
	if !strings.Contains(html, marker) {
		t.Fatalf("card action missing comfy marker:\n%s", html)
	}
	if strings.Contains(html, "data-ai-debug-privacy") {
		t.Fatalf("card action contains the removed inline privacy notice:\n%s", html)
	}
	for _, forbidden := range []string{"data-board-card-content=\"compact\" data-ai-debug-card-action", "data-board-card-content=\"cozy\" data-ai-debug-card-action", "data-board-card-content=\"comfy\" data-ai-debug-card-action"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("card action reused a board content density marker %q:\n%s", forbidden, html)
		}
	}
}

func TestAIDebugScriptFetchesPromptOnlyOnClick(t *testing.T) {
	t.Parallel()

	html := renderBoardComponent(t, aiDebugScript())
	for _, want := range []string{
		"role=\"alertdialog\"",
		"aria-labelledby=\"ai-debug-privacy-title\"",
		"aria-describedby=\"ai-debug-privacy-description\"",
		"Contains private fleet detail. Do not paste into a public or shared context.",
		"Copy prompt",
		"Don't warn me again",
		"data-ai-debug-cancel autofocus",
		"[data-ai-debug-url]",
		"event.stopPropagation()",
		"dialog.showModal()",
		"dialog.close(\"confirm\")",
		"fetchAndCopy(button, url)",
		"\"HX-Request\": \"true\"",
		"navigator.clipboard.writeText(value).catch",
		"copyFallback(value)",
		"window.localStorage && window.localStorage.getItem(privacyStorageKey) === \"true\"",
		"window.localStorage && window.localStorage.setItem(privacyStorageKey, \"true\")",
		"button.removeAttribute(\"aria-busy\")",
		"button.focus()",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("AI Debug script missing %q:\n%s", want, html)
		}
	}
}

func TestAIDebugDialogIsSharedOutsideSnapshot(t *testing.T) {
	t.Parallel()

	html := renderBoardComponent(t, ProjectBoardPage(boardTestData()))
	dialogAt := strings.Index(html, "id=\"ai-debug-privacy-dialog\"")
	snapshotAt := strings.Index(html, "id=\"snapshot\"")
	if dialogAt < 0 || snapshotAt < 0 || dialogAt >= snapshotAt {
		t.Fatal("shared AI Debug dialog is not rendered before the live snapshot")
	}
	if got := strings.Count(html, "id=\"ai-debug-privacy-dialog\""); got != 1 {
		t.Fatalf("shared AI Debug dialog count = %d, want 1", got)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
