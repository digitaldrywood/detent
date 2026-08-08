package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector/memory"
)

func TestKanbanMoveValidationResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		form        url.Values
		status      int
		message     string
		wantStatus  int
		wantHeaders map[string]string
		wantBody    []string
	}{
		{
			name: "dialog form renders move dialog validation",
			form: url.Values{
				"kanban_dialog": {"true"},
				"current_state": {"Todo"},
				"issue_id":      {"I_kw1"},
				"identifier":    {"digitaldrywood/detent#1"},
				"title":         {"Dialog card"},
			},
			status:     http.StatusBadRequest,
			message:    "Target state is required.",
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"HX-Retarget": kanbanDialogContentTarget,
				"HX-Reswap":   "innerHTML",
			},
			wantBody: []string{
				"Target state is required.",
				`hx-post="/api/v1/kanban/move"`,
			},
		},
		{
			name:       "non-dialog form returns move feedback",
			form:       url.Values{},
			status:     http.StatusUnprocessableEntity,
			message:    "Move is not allowed.",
			wantStatus: http.StatusUnprocessableEntity,
			wantBody: []string{
				`id="kanban-feedback"`,
				"Move is not allowed.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := newKanbanValidationResponseTestServer()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/kanban/move", strings.NewReader(tt.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
			c := echo.New().NewContext(req, rec)

			if err := server.kanbanMoveValidationResponse(c, tt.status, tt.message); err != nil {
				t.Fatalf("kanbanMoveValidationResponse() error = %v", err)
			}
			assertKanbanValidationResponse(t, rec, tt.wantStatus, tt.wantHeaders, tt.wantBody)
		})
	}
}

func TestKanbanCommentValidationResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		form        url.Values
		status      int
		message     string
		wantStatus  int
		wantHeaders map[string]string
		wantBody    []string
	}{
		{
			name: "dialog form renders comment dialog validation",
			form: url.Values{
				"kanban_dialog": {"true"},
				"target":        {"issue"},
				"issue_id":      {"I_kw1"},
				"identifier":    {"digitaldrywood/detent#1"},
				"title":         {"Dialog card"},
			},
			status:     http.StatusBadRequest,
			message:    "Comment body is required.",
			wantStatus: http.StatusOK,
			wantHeaders: map[string]string{
				"HX-Retarget": kanbanDialogContentTarget,
				"HX-Reswap":   "innerHTML",
			},
			wantBody: []string{
				"Comment body is required.",
				`hx-post="/api/v1/kanban/comment"`,
			},
		},
		{
			name:       "non-dialog form returns comment feedback",
			form:       url.Values{},
			status:     http.StatusBadRequest,
			message:    "Comment body is required.",
			wantStatus: http.StatusBadRequest,
			wantBody: []string{
				`id="kanban-feedback"`,
				"Comment body is required.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := newKanbanValidationResponseTestServer()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/kanban/comment", strings.NewReader(tt.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
			c := echo.New().NewContext(req, rec)

			if err := server.kanbanCommentValidationResponse(c, tt.status, tt.message); err != nil {
				t.Fatalf("kanbanCommentValidationResponse() error = %v", err)
			}
			assertKanbanValidationResponse(t, rec, tt.wantStatus, tt.wantHeaders, tt.wantBody)
		})
	}
}

func TestKanbanFeedbackSuccessTriggerTiming(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		wantTrigger string
	}{
		{
			name:        "success triggers after swap",
			status:      http.StatusOK,
			wantTrigger: kanbanDialogSucceeded,
		},
		{
			name:   "validation error does not trigger success",
			status: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/kanban/move", nil)
			req.Header.Set("HX-Request", "true")
			c := echo.New().NewContext(req, rec)

			if err := kanbanFeedback(c, tt.status, "feedback"); err != nil {
				t.Fatalf("kanbanFeedback() error = %v", err)
			}
			if got := rec.Header().Get(kanbanSuccessTriggerHeader); got != tt.wantTrigger {
				t.Fatalf("%s = %q, want %q", kanbanSuccessTriggerHeader, got, tt.wantTrigger)
			}
			if got := rec.Header().Get("HX-Trigger"); got != "" {
				t.Fatalf("HX-Trigger = %q, want empty", got)
			}
		})
	}
}

func newKanbanValidationResponseTestServer() *Server {
	workflow := workflowconfig.Default()
	workflow.Server.Kanban = workflowconfig.Kanban{Mode: workflowconfig.KanbanModeIntegration}
	return &Server{
		connector:      memory.New(memory.Config{}),
		kanban:         workflowconfig.Kanban{Mode: workflowconfig.KanbanModeIntegration},
		kanbanWorkflow: workflow,
	}
}

func assertKanbanValidationResponse(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantHeaders map[string]string, wantBody []string) {
	t.Helper()

	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	for header, want := range wantHeaders {
		if got := rec.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	body := rec.Body.String()
	for _, want := range wantBody {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}
