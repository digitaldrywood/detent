package hubserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/labstack/echo/v4"
)

// This is the shared connection approval surface, not a conversation control.
// Browser session authentication and existing hosted CSRF are both mandatory.
func (s *Service) workspaceApprovalAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if s.config.Hosted == nil || c.Request().Header.Get(echo.HeaderAuthorization) != "" {
			return echo.NewHTTPError(http.StatusForbidden, "Operator session required")
		}
		credential, _, err := s.hostedCredential(c)
		if err != nil || credential.Hosted == nil {
			return echo.NewHTTPError(http.StatusForbidden, "Operator session required")
		}
		if c.Request().Method == http.MethodPost && !s.hostedCSRFValid(c) {
			return echo.NewHTTPError(http.StatusForbidden, "Invalid operator decision")
		}
		return s.operatorAuthority(next)(c)
	}
}
func (s *Service) workspaceApprovalURL(id string) string {
	base := ""
	if s.config.Hosted != nil {
		base = strings.TrimRight(s.config.Hosted.PublicURL, "/")
	}
	return base + s.hostedPath("/chat/approval") + "?connection_id=" + url.QueryEscape(id)
}
func (s *Service) workspaceDecisionToken(c echo.Context, id, actionID string) string {
	var action chat.Action
	if actionID != "" {
		var ok bool
		action, ok = s.operatorChat.Action(id, actionID)
		if !ok {
			return ""
		}
	}
	raw, err := json.Marshal(struct {
		Browser    operatortool.Identity
		Connection string
		Action     chat.Action
	}{operatortool.ConnectionIdentity(c.Request().Context()), id, action})
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(s.hostedPageCSRF(c)))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *Service) workspaceApprovalPage(c echo.Context) error {
	id := c.QueryParam("connection_id")
	conversation := s.operatorChat.Conversation(id)
	if conversation.ConnectionID == "" || conversation.OrganizationID != operatortool.ConnectionIdentity(c.Request().Context()).OrganizationID {
		return echo.NewHTTPError(http.StatusNotFound, "Connection unavailable")
	}
	type preview struct {
		ID, Summary, Arguments, Token, Command string
		Pending                                bool
	}
	data := struct {
		ID, CSRF, ModeToken, Client, Organization, Mode string
		Actions                                         []preview
	}{ID: id, CSRF: s.hostedPageCSRF(c), ModeToken: s.workspaceDecisionToken(c, id, ""), Client: conversation.Client, Organization: conversation.OrganizationID, Mode: string(conversation.Mode)}
	for _, action := range conversation.Actions {
		var request workspaceToolRequest
		if operatortool.DecodeArguments(action.Arguments, &request) != nil {
			continue
		}
		if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), workspaceRequirement(string(action.Kind), request, false)); err != nil {
			continue
		}
		data.Actions = append(data.Actions, preview{ID: action.ID, Summary: chat.ActionSummary(action), Arguments: string(action.Arguments), Token: s.workspaceDecisionToken(c, id, action.ID), Pending: action.Status == chat.ActionPending, Command: action.Description})
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Content-Type", "text/html; charset=utf-8")
	return workspaceApprovalTemplate.Execute(c.Response(), data)
}

var workspaceApprovalTemplate = template.Must(template.New("approval").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Detent operator decisions</title><main><h1>Operator decisions</h1><p>Client: {{.Client}} · Organization: {{.Organization}}</p>{{range .Actions}}<article><h2>{{.Summary}}</h2><pre>{{.Arguments}}</pre>{{if .Command}}<p>Configured command:</p><pre>{{.Command}}</pre>{{end}}{{if .Pending}}<form method="post"><input type="hidden" name="connection_id" value="{{$.ID}}"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="form_token" value="{{.Token}}"><input type="hidden" name="action_id" value="{{.ID}}"><button name="decision" value="confirm">Confirm</button><button name="decision" value="reject">Reject</button></form>{{end}}</article>{{end}}<form method="post"><input type="hidden" name="connection_id" value="{{.ID}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="form_token" value="{{.ModeToken}}"><input type="hidden" name="decision" value="mode"><label>Connection mode <select name="mode"><option value="confirmation" {{if eq .Mode "confirmation"}}selected{{end}}>Confirm material actions</option><option value="yolo" {{if eq .Mode "yolo"}}selected{{end}}>YOLO</option></select></label><button>Apply</button></form></main></html>`))

func (s *Service) workspaceApprovalDecision(c echo.Context) error {
	id, actionID := c.FormValue("connection_id"), c.FormValue("action_id")
	expected := s.workspaceDecisionToken(c, id, actionID)
	if expected == "" || !hmac.Equal([]byte(expected), []byte(c.FormValue("form_token"))) {
		return echo.NewHTTPError(http.StatusForbidden, "Invalid operator decision")
	}
	ctx := c.Request().Context()
	if actionID != "" {
		action, ok := s.operatorChat.Action(id, actionID)
		if !ok {
			return echo.NewHTTPError(http.StatusNotFound, "Action unavailable")
		}
		var r workspaceToolRequest
		if operatortool.DecodeArguments(action.Arguments, &r) != nil {
			return echo.NewHTTPError(http.StatusForbidden, "Action unavailable")
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, workspaceRequirement(string(action.Kind), r, true)); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "Action unavailable")
		}
	}
	ctx = chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx))
	var err error
	switch c.FormValue("decision") {
	case "confirm":
		_, err = s.operatorChat.Confirm(ctx, id, actionID)
	case "reject":
		_, err = s.operatorChat.RejectConnectionAction(ctx, id, actionID)
	case "mode":
		if _, checkErr := operatortool.AuthorizeCurrent(c.Request().Context(), operatortool.Requirement{Scope: apikey.ScopeWrite}); checkErr != nil {
			return echo.NewHTTPError(http.StatusForbidden, "Operator access required")
		}
		err = s.operatorChat.SetConnectionMode(ctx, id, chat.ConnectionMode(c.FormValue("mode")))
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid operator decision")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, "Operator decision unavailable")
	}
	return c.Redirect(http.StatusSeeOther, s.workspaceApprovalURL(id))
}
