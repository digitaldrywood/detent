package hubserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/web/templates"
	"github.com/labstack/echo/v4"
)

func (s *Service) hostedOperatorBrowser(c echo.Context, next echo.HandlerFunc) error {
	if s.config.Hosted == nil || c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
	}
	return s.operatorAuthority(func(c echo.Context) error {
		credential, err := currentHubOperator(c.Request().Context())
		if err != nil || credential.SessionHash == "" || credential.Hosted == nil || s.operatorFormCSRF(c) == "" {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		return next(c)
	})(c)
}

func (s *Service) operatorFormCSRF(c echo.Context) string {
	if s.hostedShared() {
		return s.hostedSharedCSRF(c)
	}
	cookie, err := c.Cookie(hostedCookie)
	if err != nil {
		return ""
	}
	return hostedCSRF(cookie.Value)
}

// Reuse the authenticated dashboard session's form secret and the existing
// approval conversation. Bind each decision to what this browser displayed.
func (s *Service) billingDecisionToken(c echo.Context, id, actionID string) string {
	conversation := s.operatorChat.Conversation(id)
	var action chat.Action
	if actionID != "" {
		action, _ = s.operatorChat.Action(id, actionID)
	}
	raw, _ := json.Marshal(struct {
		ID       string
		Identity operatortool.Identity
		Mode     chat.ConnectionMode
		Action   chat.Action
	}{id, operatortool.ConnectionIdentity(c.Request().Context()), conversation.Mode, action})
	mac := hmac.New(sha256.New, []byte(s.operatorFormCSRF(c)))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) hostedOperatorApproval(c echo.Context) error {
	return s.hostedOperatorBrowser(c, func(c echo.Context) error {
		return s.renderBillingApproval(c, strings.TrimSpace(c.QueryParam("connection_id")))
	})
}

func (s *Service) renderBillingApproval(c echo.Context, id string) error {
	if id == "" || len(id) > 256 {
		return echo.NewHTTPError(http.StatusNotFound, "Connection is unavailable")
	}
	conversation := s.operatorChat.Conversation(id)
	if conversation.ConnectionID == "" || conversation.OrganizationID != operatortool.ConnectionIdentity(c.Request().Context()).OrganizationID {
		return echo.NewHTTPError(http.StatusNotFound, "Connection is unavailable")
	}
	if err := s.authorizeOperatorBrowserActions(c, id, conversation.Actions); err != nil {
		return err
	}
	tokens := make(map[string]string, len(conversation.Actions))
	for _, action := range conversation.Actions {
		tokens[action.ID] = s.billingDecisionToken(c, id, action.ID)
	}
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "same-origin")
	return templates.ChatApproval(templates.ChatData{Conversation: conversation, FormToken: s.billingDecisionToken(c, id, ""), ActionTokens: tokens, ApprovalPath: s.hostedPath("/chat/approval"), CSRF: s.operatorFormCSRF(c)}).Render(c.Request().Context(), c.Response().Writer)
}

func (s *Service) hostedOperatorDecision(c echo.Context) error {
	return s.hostedOperatorBrowser(c, func(c echo.Context) error {
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, 4096)
		id, actionID := c.FormValue("connection_id"), c.FormValue("action_id")
		if id == "" || len(id) > 256 || len(actionID) > 256 || !s.hostedCSRFValid(c) || !hmac.Equal([]byte(c.FormValue("form_token")), []byte(s.billingDecisionToken(c, id, actionID))) {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		conversation := s.operatorChat.Conversation(id)
		if conversation.ConnectionID == "" || conversation.OrganizationID != operatortool.ConnectionIdentity(c.Request().Context()).OrganizationID {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		actions := conversation.Actions
		if actionID != "" {
			action, ok := s.operatorChat.Action(id, actionID)
			if !ok {
				return echo.NewHTTPError(http.StatusNotFound, "Action is unavailable")
			}
			actions = []chat.Action{action}
		}
		if err := s.authorizeOperatorBrowserActions(c, id, actions); err != nil {
			return err
		}
		ctx := chat.WithOperatorApproval(c.Request().Context(), operatortool.ConnectionIdentity(c.Request().Context()))
		var err error
		switch c.FormValue("decision") {
		case "confirm":
			_, err = s.operatorChat.Confirm(ctx, id, actionID)
		case "reject":
			_, err = s.operatorChat.RejectConnectionAction(ctx, id, actionID)
		case "mode":
			err = s.operatorChat.SetConnectionMode(ctx, id, chat.ConnectionMode(c.FormValue("mode")))
		default:
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid operator decision")
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusConflict, "Operator decision is unavailable; refresh the preview")
		}
		return c.Redirect(http.StatusSeeOther, s.hostedPath("/chat/approval")+"?connection_id="+id)
	})
}

// Administration approvals use the originating browser and the exact operation's
// powers. Billing approvals retain the application's owner-only policy.
func (s *Service) authorizeOperatorBrowserActions(c echo.Context, id string, actions []chat.Action) error {
	for _, action := range actions {
		if operatortool.IsAdministration(string(action.Kind)) {
			if err := s.operatorChat.CheckBrowserSession(c.Request().Context(), id); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
			in, err := operatoradmin.Decode(string(action.Kind), action.Arguments)
			if err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
			if err := s.administration.App.Authorize(c.Request().Context(), string(action.Kind), in, ""); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
		} else if _, ok := operatortool.ChangeDefinition(string(action.Kind)); ok {
			if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), operatortool.Requirement{Scope: apikey.ScopeAdmin, ProjectID: action.ProjectID}); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
		} else if _, err := operatortool.WorkspaceDefinition(string(action.Kind)); err == nil {
			var request workspaceToolRequest
			if operatortool.DecodeArguments(action.Arguments, &request) != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
			requirement := workspaceRequirement(string(action.Kind), request, true)
			if action.Kind == "delete_project_action" && action.Status == chat.ActionSucceeded {
				requirement.ResourceKind, requirement.ResourceID = "", ""
			}
			if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), requirement); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
		} else if hubFleetTool(string(action.Kind)) {
			if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), hubFleetRequirement(string(action.Kind))); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
		} else if _, err := s.hostedBillingOwner(c); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
	}
	return nil
}
