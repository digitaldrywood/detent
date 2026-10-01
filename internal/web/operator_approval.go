package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const operatorConnectionHeader = "X-Detent-Connection-ID"

func (s *Server) apiOperatorConnection(c echo.Context) error {
	// This establishes a default-confirmation stdio bridge identity. There is
	// deliberately no mode argument; only the browser operator chooses YOLO.

	var data [24]byte
	if _, err := rand.Read(data[:]); err != nil {
		return err
	}
	id := "stdio-" + hex.EncodeToString(data[:])
	ctx := operatortool.BindConnection(c.Request().Context(), id, "local MCP stdio")
	if err := (dashboardOperatorExecutor{server: s}).OpenConnection(ctx); err != nil {
		return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
	}
	return c.JSON(http.StatusCreated, struct {
		ID  string `json:"connection_id"`
		URL string `json:"setup_url"`
	}{id, s.operatorApprovalURL(id)})
}

// The dashboard's browser authentication is the human boundary. API credentials
// cannot fetch the form secret or submit decisions, even with forged HX headers.
func (s *Server) operatorBrowserReadAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if requestAPICredentialsSupplied(c.Request()) {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		// A public dashboard UI cookie is not human authentication. Reuse only
		// an existing login or private dashboard session for operator decisions.
		if _, authenticated := webSessionFromContext(c.Request().Context()); !authenticated && !s.authorizePrivateDashboardSession(c) {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		if _, err := s.authorizeAPIRequest(c, apiAuthOptions{mutating: c.Request().Method != http.MethodGet}); err != nil {
			return err
		}

		return s.operatorAuthority(next)(c)
	}
}

func (s *Server) operatorBrowserFormAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return s.operatorBrowserReadAuth(func(c echo.Context) error {
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, 4096)
		cookie, err := c.Cookie(apiKeyDashboardCookieName)
		if err != nil || !requestSameOriginDashboardSource(c.Request()) || !apiStaticTokenEqual(c.FormValue("form_token"), s.operatorDecisionToken(c, c.FormValue("connection_id"), c.FormValue("action_id"))) || !apiStaticTokenEqual(cookie.Value, s.apiKeyDashboardToken()) {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		if s.authorizePrivateDashboardSession(c) && !s.dashboardAccess().AllowWrite {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		return next(c)
	})
}

func (s *Server) operatorApprovalPage(c echo.Context) error {
	return s.renderOperatorApproval(c, strings.TrimSpace(c.QueryParam("connection_id")), "")
}

func (s *Server) renderOperatorApproval(c echo.Context, id, message string) error {
	conversation := s.chat.Conversation(id)
	if conversation.ConnectionID == "" || conversation.OrganizationID != operatortool.ConnectionIdentity(c.Request().Context()).OrganizationID {
		return echo.NewHTTPError(http.StatusNotFound, "Connection is unavailable")
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "same-origin")
	token := s.apiKeyDashboardToken()
	s.setAPIKeyDashboardCookie(c, token, "/chat/approval")
	tokens := make(map[string]string, len(conversation.Actions))
	for _, action := range conversation.Actions {
		tokens[action.ID] = s.operatorDecisionToken(c, id, action.ID)
	}
	return render(c, templates.ChatApproval(templates.ChatData{Conversation: conversation, Error: message, FormToken: s.operatorDecisionToken(c, id, ""), ActionTokens: tokens}))
}

func (s *Server) operatorApprovalDecision(c echo.Context) error {
	id := strings.TrimSpace(c.FormValue("connection_id"))
	conversation := s.chat.Conversation(id)
	if conversation.ConnectionID == "" {
		return echo.NewHTTPError(http.StatusNotFound, "Connection is unavailable")
	}
	ctx := c.Request().Context()
	// The approving browser must have current write access to the target too.
	if actionID := c.FormValue("action_id"); actionID != "" {
		action, ok := s.chat.Action(id, actionID)
		if !ok {
			return echo.NewHTTPError(http.StatusNotFound, "Action is unavailable")
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, s.fleetMutationRequirement(string(action.Kind), action.ProjectID)); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		credential, ok := apiCredentialFromContext(ctx)
		if !ok || !apikey.HasScope(credential.Scopes, apikey.ScopeWrite) || !apikey.AllowsProject(credential.ProjectIDs, action.ProjectID) {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
	}
	ctx = chatpkg.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx))
	var err error
	switch c.FormValue("decision") {
	case "confirm":
		_, err = s.chat.Confirm(ctx, id, c.FormValue("action_id"))
	case "reject":
		_, err = s.chat.RejectConnectionAction(ctx, id, c.FormValue("action_id"))
	case "mode":
		err = s.chat.SetConnectionMode(ctx, id, chatpkg.ConnectionMode(c.FormValue("mode")))
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid operator decision")
	}
	message := ""
	if err != nil {
		message = "The operator decision could not be applied. Refresh the connection or preview and try again."
		return s.renderOperatorApproval(c, id, message)
	}
	return c.Redirect(http.StatusSeeOther, "/chat/approval?connection_id="+id)
}

// Bind the existing dashboard form secret to exactly what the browser displayed,
// including action, organization/resource, originating client and browser identity.
func (s *Server) operatorDecisionToken(c echo.Context, id, actionID string) string {
	conversation := s.chat.Conversation(id)
	if conversation.ConnectionID == "" {
		return ""
	}
	var action chatpkg.Action
	if actionID != "" {
		var ok bool
		action, ok = s.chat.Action(id, actionID)
		if !ok {
			return ""
		}
	}
	payload, err := json.Marshal(struct {
		Browser                            operatortool.Identity
		ConnectionID, Organization, Client string
		Mode                               chatpkg.ConnectionMode
		Action                             chatpkg.Action
	}{operatortool.ConnectionIdentity(c.Request().Context()), id, conversation.OrganizationID, conversation.Client, conversation.Mode, action})
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, s.dashboardAuthSecret[:])
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
