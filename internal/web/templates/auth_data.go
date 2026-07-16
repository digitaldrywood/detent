package templates

import "net/url"

type AuthPageState string

const (
	AuthPageSignIn      AuthPageState = "sign_in"
	AuthPageCheckInbox  AuthPageState = "check_inbox"
	AuthPageInvalidLink AuthPageState = "invalid_link"
	AuthPageUnavailable AuthPageState = "unavailable"
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
	case AuthPageUnavailable:
		return "Sign-in unavailable"
	default:
		return "Sign in"
	}
}

func authRetryPath(next string) string {
	if next == "" || next == "/" {
		return "/login"
	}
	return "/login?next=" + url.QueryEscape(next)
}
