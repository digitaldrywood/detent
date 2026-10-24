package templates

import "net/url"

type AuthPageState string

const (
	AuthPageSignIn          AuthPageState = "sign_in"
	AuthPageOIDCSignIn      AuthPageState = "oidc_sign_in"
	AuthPageCheckInbox      AuthPageState = "check_inbox"
	AuthPageInvalidLink     AuthPageState = "invalid_link"
	AuthPageInvalidIdentity AuthPageState = "invalid_identity"
	AuthPageDenied          AuthPageState = "denied"
	AuthPageUnavailable     AuthPageState = "unavailable"
)

type AuthPageData struct {
	Assets AssetPaths
	Next   string
	State  AuthPageState
}

func authPageTitle(state AuthPageState) string {
	switch state {
	case AuthPageCheckInbox:
		return "Check your inbox"
	case AuthPageInvalidLink:
		return "Link unavailable"
	case AuthPageInvalidIdentity:
		return "Identity unavailable"
	case AuthPageDenied:
		return "Access denied"
	case AuthPageUnavailable:
		return "Sign-in unavailable"
	default:
		return "Sign in"
	}
}

func authOIDCStartPath(next string) string {
	if next == "" || next == "/" {
		return "/auth/oidc/start"
	}
	return "/auth/oidc/start?next=" + url.QueryEscape(next)
}

func authRetryPath(next string) string {
	if next == "" || next == "/" {
		return "/login"
	}
	return "/login?next=" + url.QueryEscape(next)
}
