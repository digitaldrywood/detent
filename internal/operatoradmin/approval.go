package operatoradmin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

// Approval is called only by a deployment's existing browser authentication
// and CSRF middleware. Neither MCP nor an API credential can invoke it.
func (e *Executor) Approval(c echo.Context, csrf string) error {
	ctx := c.Request().Context()
	identity := operatortool.ConnectionIdentity(ctx)
	id := c.QueryParam("connection_id")
	if c.Request().Method == http.MethodPost {
		id = c.FormValue("connection_id")
	}
	if err := e.Chat.CheckBrowserSession(ctx, id); err != nil {
		return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
	}
	conversation := e.Chat.Conversation(id)
	if !identity.Valid() || conversation.ConnectionID == "" || conversation.OrganizationID != identity.OrganizationID {
		return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
	}
	token := func(actionID string) string {
		raw, err := json.Marshal(struct {
			Identity     operatortool.Identity
			Conversation chat.Conversation
			ActionID     string
		}{identity, conversation, actionID})
		if err != nil {
			return ""
		}
		mac := hmac.New(sha256.New, []byte(csrf))
		_, _ = mac.Write(raw)
		return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	message := ""
	if c.Request().Method == http.MethodPost {
		actionID := c.FormValue("action_id")
		if csrf == "" || !hmac.Equal([]byte(c.FormValue("form_token")), []byte(token(actionID))) {
			return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		}
		// A browser also needs current powers over the exact operation it approves.
		if actionID != "" {
			action, ok := e.Chat.Action(id, actionID)
			if !ok {
				return echo.NewHTTPError(http.StatusNotFound, ErrUnavailable.Error())
			}
			in, err := Decode(string(action.Kind), action.Arguments)
			if err != nil {
				return echo.NewHTTPError(http.StatusForbidden, ErrUnavailable.Error())
			}
			if err := e.authorize(ctx, string(action.Kind), in, ""); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, operatortool.ErrAccessDenied.Error())
			}
		}
		human := chat.WithOperatorApproval(ctx, identity)
		var err error
		switch c.FormValue("decision") {
		case "confirm":
			_, err = e.Chat.Confirm(human, id, actionID)
		case "reject":
			_, err = e.Chat.RejectConnectionAction(human, id, actionID)
		case "mode":
			err = e.Chat.SetConnectionMode(human, id, chat.ConnectionMode(c.FormValue("mode")))
		default:
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid decision")
		}
		if err == nil {
			return c.Redirect(http.StatusSeeOther, "/chat/approval?connection_id="+id)
		}
		message = "The decision could not be applied. Refresh the preview and try again."
		conversation = e.Chat.Conversation(id)
	}
	tokens := make(map[string]string)
	for _, action := range conversation.Actions {
		tokens[action.ID] = token(action.ID)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "no-referrer")
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	return templates.ChatApproval(templates.ChatData{Conversation: conversation, Error: message, FormToken: token(""), ActionTokens: tokens, CSRF: csrf}).Render(ctx, c.Response())
}
