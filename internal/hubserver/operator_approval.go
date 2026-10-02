package hubserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/web/templates"
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
			if !hmac.Equal([]byte(c.FormValue("form_token")), []byte(s.billingDecisionToken(c, c.FormValue("connection_id"), c.FormValue("action_id")))) {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
		}
		id := c.QueryParam("connection_id")
		if c.Request().Method == http.MethodPost {
			id = c.FormValue("connection_id")
		}
		if id == "" || len(id) > 256 || len(c.FormValue("action_id")) > 256 {
			return echo.NewHTTPError(http.StatusNotFound, "Connection is unavailable")
		}
		conversation := s.operatorChat.Conversation(id)
		if conversation.ConnectionID == "" || conversation.OrganizationID != identity.OrganizationID {
			return echo.NewHTTPError(http.StatusNotFound, "Connection is unavailable")
		}
		if conversation.PrincipalID != identity.PrincipalID {
			if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), operatortool.Requirement{Scope: apikey.ScopeAdmin}); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
		} else if len(conversation.Actions) == 0 {
			if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), operatortool.Requirement{Scope: apikey.ScopeWrite}); err != nil {
				if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), hubFleetRequirement(operatortool.UpdateRunnerRouting)); err != nil {
					return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
				}
			}
		}
		for _, action := range conversation.Actions {
			if err := s.authorizeOperatorPreview(c.Request().Context(), action); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
		}
		return next(c)
	})
}
func (s *Service) billingDecisionToken(c echo.Context, id, actionID string) string {
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
func (s *Service) hostedOperatorApproval(c echo.Context) error {
	id := c.QueryParam("connection_id")
	conversation := s.operatorChat.Conversation(id)
	if conversation.ConnectionID == "" || conversation.OrganizationID != operatortool.ConnectionIdentity(c.Request().Context()).OrganizationID {
		return echo.NewHTTPError(http.StatusNotFound, "Connection is unavailable")
	}
	tokens := map[string]string{}
	for _, action := range conversation.Actions {
		tokens[action.ID] = s.billingDecisionToken(c, id, action.ID)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "same-origin")
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	return templates.ChatApproval(templates.ChatData{Conversation: conversation, FormToken: s.billingDecisionToken(c, id, ""), ActionTokens: tokens, CSRF: s.hostedPageCSRF(c), ApprovalPath: s.hostedPath("/chat/approval"), ApprovalBasePath: s.hostedBase()}).Render(c.Request().Context(), c.Response())
}
func (s *Service) hostedOperatorDecision(c echo.Context) error {
	id, actionID := c.FormValue("connection_id"), c.FormValue("action_id")
	if token := s.billingDecisionToken(c, id, actionID); token == "" || !hmac.Equal([]byte(c.FormValue("form_token")), []byte(token)) {
		return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
	}
	ctx := c.Request().Context()
	wasPending := false
	if actionID != "" {
		action, ok := s.operatorChat.Action(id, actionID)
		if !ok {
			return echo.NewHTTPError(http.StatusNotFound, "Action is unavailable")
		}
		if err := s.authorizeOperatorPreview(ctx, action); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		wasPending = action.Status == chatpkg.ActionPending
	}
	ctx = chatpkg.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx))
	var err error
	switch c.FormValue("decision") {
	case "confirm":
		var resolved chatpkg.Conversation
		resolved, err = s.operatorChat.Confirm(ctx, id, actionID)
		if err == nil {
			for _, action := range resolved.Actions {
				if action.ID == actionID && action.Kind == chatpkg.ActionKind(operatortool.SessionLogout) && action.SignOut != nil {
					s.hostedSetCookie(c, hostedCookie, "", "/", time.Unix(1, 0))
					s.hostedSetCookie(c, hostedTransactionCookie, "", "/auth/oidc", time.Unix(1, 0))
					c.Response().Header().Set("Cache-Control", "no-store")
					return c.String(http.StatusOK, action.SignOut.Message())
				}
			}
		}
	case "reject":
		_, err = s.operatorChat.RejectConnectionAction(ctx, id, actionID)
	case "mode":
		err = s.operatorChat.SetConnectionMode(ctx, id, chatpkg.ConnectionMode(c.FormValue("mode")))
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid operator decision")
	}
	if wasPending {
		if action, ok := s.operatorChat.Action(id, actionID); ok && action.Status != chatpkg.ActionPending {
			if publishErr := s.publishCoordinatorDecision(ctx, action); publishErr != nil {
				return publishErr
			}
		}
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, "The operator decision could not be applied; refresh the project and preview")
	}
	return c.Redirect(http.StatusSeeOther, s.hostedPath("/chat/approval")+"?connection_id="+url.QueryEscape(id))
}

func (s *Service) authorizeOperatorPreview(ctx context.Context, action chatpkg.Action) error {
	if action.Mutation.Source == "chat" {
		_, err := s.authorizeCoordinatorAction(ctx, action)
		return err
	}
	definition, ok := operatortool.Lookup(string(action.Kind))
	if !ok {
		return operatortool.ErrAccessDenied
	}
	if definition.Meta.Toolset == "projects" && !definition.Annotations.ReadOnly {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: projectToolScope(string(action.Kind), false), ProjectID: action.ProjectID}); err != nil {
			return err
		}
		return (hubProjectExecutor{s}).authorizeCreatedProjectResult(ctx, string(action.Kind), json.RawMessage(action.Result), action.Status)
	}
	if operatortool.IsAdministration(string(action.Kind)) {
		if err := s.operatorChat.CheckBrowserSession(ctx, action.ConnectionID); err != nil {
			return err
		}
		in, err := operatoradmin.Decode(string(action.Kind), action.Arguments)
		if err != nil {
			return operatortool.ErrAccessDenied
		}
		return s.administration.App.Authorize(ctx, string(action.Kind), in, "")
	}
	if _, ok := operatortool.ChangeDefinition(string(action.Kind)); ok {
		_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, ProjectID: action.ProjectID})
		return err
	}
	if _, err := operatortool.WorkspaceDefinition(string(action.Kind)); err == nil {
		var request workspaceToolRequest
		if operatortool.DecodeArguments(action.Arguments, &request) != nil {
			return operatortool.ErrAccessDenied
		}
		requirement := workspaceRequirement(string(action.Kind), request, true)
		if action.Kind == "delete_project_action" && action.Status == chatpkg.ActionSucceeded {
			requirement.ResourceKind, requirement.ResourceID = "", ""
		}
		_, err := operatortool.AuthorizeCurrent(ctx, requirement)
		return err
	}
	if hubFleetTool(string(action.Kind)) {
		_, err := operatortool.AuthorizeCurrent(ctx, hubFleetRequirement(string(action.Kind)))
		return err
	}
	if action.Kind == chatpkg.ActionMoveItem {
		_, _, err := (nativeOperatorExecutor{service: s}).workflowAuthority(ctx, action.Arguments)
		return err
	}
	if string(action.Kind) == operatortool.BillingCheckout || string(action.Kind) == operatortool.BillingPortal {
		_, err := s.operatorBillingCredential(ctx, "billing", true)
		return err
	}
	return operatortool.ErrAccessDenied
}
