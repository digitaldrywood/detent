package codex

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *ResponseError) NativeTerminalFailure() tracker.NativeTerminalFailure {
	failure := tracker.NativeTerminalFailure{Provider: "codex", Operation: e.Request, RPCCode: &e.Code}
	var body struct {
		Error struct {
			Data json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(e.Body), &body) == nil {
		nativeRequestFailureMetadata(body.Error.Data, &failure)
	}
	nativeRequestFailureMetadata([]byte(e.Message), &failure)
	if e.Message == "input_too_large" {
		failure.ProviderCode = e.Message
	}
	if maximum, ok := strings.CutPrefix(e.Message, "Input exceeds the maximum length of "); ok {
		if limit, err := strconv.ParseInt(maximum, 10, 64); err == nil && limit > 0 {
			failure.ProviderCode = "input_too_large"
			failure.MaxChars = &limit
		}
	}
	return failure.Public()
}

func (e *TurnFailedError) NativeTerminalFailure() tracker.NativeTerminalFailure {
	return (tracker.NativeTerminalFailure{Provider: "codex", Operation: "turn/completed"}).Public()
}

func nativeRequestFailureMetadata(raw []byte, failure *tracker.NativeTerminalFailure) {
	var metadata struct {
		Code        string `json:"code"`
		Error       string `json:"error"`
		MaxChars    *int64 `json:"max_chars"`
		ActualChars *int64 `json:"actual_chars"`
	}
	if json.Unmarshal(raw, &metadata) != nil {
		return
	}
	if metadata.Code == "input_too_large" || metadata.Error == "input_too_large" {
		failure.ProviderCode = "input_too_large"
	}
	if metadata.MaxChars != nil {
		failure.MaxChars = metadata.MaxChars
	}
	if metadata.ActualChars != nil {
		failure.ActualChars = metadata.ActualChars
	}
}
