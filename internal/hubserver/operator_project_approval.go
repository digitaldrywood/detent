package hubserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/web/templates"
	"github.com/labstack/echo/v4"
)

// Reuse the existing portable chat approval form and hosted browser session /
// CSRF boundary. API tokens and shared service assertions never approve actions.
func (s *Service) operatorProjectBrowser(next echo.HandlerFunc) echo.HandlerFunc {
	return s.operatorAuthority(func(c echo.Context) error {
		identity := operatortool.ConnectionIdentity(c.Request().Context())
		if s.config.Hosted == nil || identity.SessionID == "" || c.Request().Header.Get(echo.HeaderAuthorization) != "" {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		if c.Request().Method == http.MethodPost {
			c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, 4096)
			if !hmac.Equal([]byte(c.FormValue("form_token")), []byte(s.projectDecisionToken(c, c.FormValue("connection_id"), c.FormValue("action_id")))) {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
		}
		id := c.QueryParam("connection_id")
		if c.Request().Method == http.MethodPost {
			id = c.FormValue("connection_id")
		}
		conversation := s.operatorChat.Conversation(id)
		if conversation.ConnectionID == "" || conversation.OrganizationID != identity.OrganizationID {
			return echo.NewHTTPError(http.StatusNotFound, "Connection is unavailable")
		}
		scope := apikey.ScopeWrite
		if conversation.PrincipalID != identity.PrincipalID {
			scope = apikey.ScopeAdmin
		}
		if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), operatortool.Requirement{Scope: scope}); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		for _, action := range conversation.Actions {
			if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), operatortool.Requirement{Scope: projectToolScope(string(action.Kind), false), ProjectID: action.ProjectID}); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
			if err := (hubProjectExecutor{s}).authorizeCreatedProjectResult(c.Request().Context(), string(action.Kind), json.RawMessage(action.Result), action.Status); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
		}
		return next(c)
	})
}
func (s *Service) projectDecisionToken(c echo.Context, id, actionID string) string {
	conversation := s.operatorChat.Conversation(id)
	if conversation.ConnectionID == "" || conversation.OrganizationID != operatortool.ConnectionIdentity(c.Request().Context()).OrganizationID {
		return ""
	}
	var action chatpkg.Action
	if actionID != "" {
		var ok bool
		action, ok = s.operatorChat.Action(id, actionID)
		if !ok {
			return ""
		}
	}
	payload, err := json.Marshal(struct {
		Browser              operatortool.Identity
		ConnectionID, Client string
		Mode                 chatpkg.ConnectionMode
		Action               chatpkg.Action
	}{operatortool.ConnectionIdentity(c.Request().Context()), id, conversation.Client, conversation.Mode, action})
	if err != nil {
		return ""
	}
	secret := s.hostedPageCSRF(c)
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Service) projectApprovalPage(c echo.Context) error {
	id := c.QueryParam("connection_id")
	conversation := s.operatorChat.Conversation(id)
	if conversation.ConnectionID == "" || conversation.OrganizationID != operatortool.ConnectionIdentity(c.Request().Context()).OrganizationID {
		return echo.NewHTTPError(http.StatusNotFound, "Connection is unavailable")
	}
	tokens := map[string]string{}
	for _, action := range conversation.Actions {
		tokens[action.ID] = s.projectDecisionToken(c, id, action.ID)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	return templates.ChatApproval(templates.ChatData{Conversation: conversation, FormToken: s.projectDecisionToken(c, id, ""), ActionTokens: tokens, ApprovalCSRF: s.hostedPageCSRF(c), ApprovalBasePath: s.hostedBase()}).Render(c.Request().Context(), c.Response())
}
func (s *Service) projectApprovalDecision(c echo.Context) error {
	id, actionID := c.FormValue("connection_id"), c.FormValue("action_id")
	if token := s.projectDecisionToken(c, id, actionID); token == "" || !hmac.Equal([]byte(c.FormValue("form_token")), []byte(token)) {
		return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
	}
	ctx := c.Request().Context()
	if actionID != "" {
		action, ok := s.operatorChat.Action(id, actionID)
		if !ok {
			return echo.NewHTTPError(http.StatusNotFound, "Action is unavailable")
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: projectToolScope(string(action.Kind), false), ProjectID: action.ProjectID}); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
	}
	ctx = chatpkg.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx))
	var err error
	switch c.FormValue("decision") {
	case "confirm":
		_, err = s.operatorChat.Confirm(ctx, id, actionID)
	case "reject":
		_, err = s.operatorChat.RejectConnectionAction(ctx, id, actionID)
	case "mode":
		err = s.operatorChat.SetConnectionMode(ctx, id, chatpkg.ConnectionMode(c.FormValue("mode")))
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid operator decision")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, "The operator decision could not be applied; refresh the project and preview")
	}
	return c.Redirect(http.StatusSeeOther, s.hostedPath("/chat/approval")+"?connection_id="+url.QueryEscape(id))
}

func (s *Service) projectApprovalBaseURL() string {
	if s.config.Hosted == nil {
		return ""
	}
	return strings.TrimRight(s.config.Hosted.PublicURL, "/") + s.hostedBase()
}
