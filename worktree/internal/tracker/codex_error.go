package tracker

import "strings"

func CodexErrorCode(code string) string {
	switch strings.ToLower(strings.ReplaceAll(code, "_", "")) {
	case "usagelimitexceeded":
		return "usage_limit_exceeded"
	case "contextwindowexceeded":
		return "context_window_exceeded"
	case "badrequest":
		return "bad_request"
	case "unauthorized":
		return "unauthorized"
	case "sandboxerror":
		return "sandbox_error"
	case "serveroverloaded":
		return "server_overloaded"
	case "httpconnectionfailed":
		return "http_connection_failed"
	case "responsestreamconnectionfailed":
		return "response_stream_connection_failed"
	case "responsestreamdisconnected":
		return "response_stream_disconnected"
	case "responsestreamtoomanyfailedattempts":
		return "response_stream_too_many_failed_attempts"
	default:
		return "other"
	}
}
