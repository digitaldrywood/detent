package hubserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/web/templates"
	"github.com/labstack/echo/v4"
)

// Use the existing dashboard approval view and hosted browser authentication.
// Neither API tokens nor MCP messages can approve actions or select YOLO.
func (s *Service) hubOperatorBrowser(c echo.Context) (string, error) {
	if s.config.Hosted == nil || c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		return "", operatortool.ErrAccessDenied
	}
	credential, err := currentHubOperator(c.Request().Context())
	if err != nil || credential.SessionHash == "" || credential.Hosted == nil {
		return "", operatortool.ErrAccessDenied
	}
	if s.hostedShared() {
		return s.hostedSharedCSRF(c), nil
	}
	cookie, err := c.Cookie(hostedCookie)
	if err != nil {
		return "", operatortool.ErrAccessDenied
	}
	return hostedCSRF(cookie.Value), nil
}
func (s *Service) hubOperatorFormToken(c echo.Context, id, actionID, secret string) string {
	conversation := s.operatorActions.Conversation(id)
	var action chatpkg.Action
	if actionID != "" {
		var ok bool
		action, ok = s.operatorActions.Action(id, actionID)
		if !ok {
			return ""
		}
	}
	raw, err := json.Marshal(struct {
		Identity operatortool.Identity
		ID       string
		Mode     chatpkg.ConnectionMode
		Action   chatpkg.Action
	}{operatortool.ConnectionIdentity(c.Request().Context()), id, conversation.Mode, action})
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Service) hubOperatorApprovalPage(c echo.Context) error {
	secret, err := s.hubOperatorBrowser(c)
	if err != nil || secret == "" {
		return c.NoContent(http.StatusForbidden)
	}
	id := strings.TrimSpace(c.QueryParam("connection_id"))
	conversation := s.operatorActions.Conversation(id)
	if conversation.ConnectionID == "" || conversation.OrganizationID != operatortool.ConnectionIdentity(c.Request().Context()).OrganizationID {
		return c.NoContent(http.StatusNotFound)
	}
	if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), hubFleetRequirement(operatortool.UpdateRunnerRouting)); err != nil {
		return c.NoContent(http.StatusForbidden)
	}
	tokens := map[string]string{}
	for _, a := range conversation.Actions {
		tokens[a.ID] = s.hubOperatorFormToken(c, id, a.ID, secret)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "same-origin")
	c.Response().Header().Set("Content-Type", "text/html; charset=utf-8")
	return templates.ChatApproval(templates.ChatData{Conversation: conversation, FormToken: s.hubOperatorFormToken(c, id, "", secret), ActionTokens: tokens}).Render(c.Request().Context(), c.Response())
}
func (s *Service) hubOperatorApprovalDecision(c echo.Context) error {
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, 4096)
	secret, err := s.hubOperatorBrowser(c)
	if err != nil || secret == "" {
		return c.NoContent(http.StatusForbidden)
	}
	id, actionID := c.FormValue("connection_id"), c.FormValue("action_id")
	conversation := s.operatorActions.Conversation(id)
	if conversation.ConnectionID == "" || conversation.OrganizationID != operatortool.ConnectionIdentity(c.Request().Context()).OrganizationID {
		return c.NoContent(http.StatusForbidden)
	}
	expected := s.hubOperatorFormToken(c, id, actionID, secret)
	if expected == "" || subtle.ConstantTimeCompare([]byte(c.FormValue("form_token")), []byte(expected)) != 1 {
		return c.NoContent(http.StatusForbidden)
	}
	if _, err := operatortool.AuthorizeCurrent(c.Request().Context(), hubFleetRequirement(operatortool.UpdateRunnerRouting)); err != nil {
		return c.NoContent(http.StatusForbidden)
	}
	ctx := chatpkg.WithOperatorApproval(c.Request().Context(), operatortool.ConnectionIdentity(c.Request().Context()))
	switch c.FormValue("decision") {
	case "confirm":
		_, err = s.operatorActions.Confirm(ctx, id, actionID)
	case "reject":
		_, err = s.operatorActions.RejectConnectionAction(ctx, id, actionID)
	case "mode":
		err = s.operatorActions.SetConnectionMode(ctx, id, chatpkg.ConnectionMode(c.FormValue("mode")))
	default:
		return c.NoContent(http.StatusBadRequest)
	}
	if err != nil {
		return c.JSON(http.StatusConflict, apiErrorResponse{Code: "action_unavailable", Message: "Operator action could not be applied"})
	}
	return c.Redirect(http.StatusSeeOther, "/chat/approval?connection_id="+url.QueryEscape(id))
}
