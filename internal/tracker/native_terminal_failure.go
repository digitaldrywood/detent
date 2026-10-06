package tracker

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

type NativeTerminalFailure struct {
	Error            string    `json:"error,omitempty"`
	ErrorClass       string    `json:"error_class,omitempty"`
	TurnStartRefused bool      `json:"turn_start_refused,omitempty"`
	ObservedAt       time.Time `json:"observed_at"`
	Source           string    `json:"source"`
	Coverage         string    `json:"coverage"`
	Unavailable      []string  `json:"unavailable"`
	Provider         string    `json:"provider,omitempty"`
	Operation        string    `json:"operation,omitempty"`
	RPCCode          *int      `json:"rpc_code,omitempty"`
	ProviderCode     string    `json:"provider_code,omitempty"`
	MaxChars         *int64    `json:"max_chars,omitempty"`
	ActualChars      *int64    `json:"actual_chars,omitempty"`
	Summary          string    `json:"summary"`
}

func (f NativeTerminalFailure) Public() NativeTerminalFailure {
	text := NativeFinalization{}
	first, _, _ := strings.Cut(f.Error, "\n")
	f.Error = text.publicText(strings.TrimSpace(first))
	switch f.ErrorClass {
	case "backend_startup", "protocol", "workspace_hook":
	default:
		f.ErrorClass = ""
	}
	f.Source = "host_runner_completion"
	f.Coverage = "recorded_execution_error_only; not_change_publication_or_issue_acceptance_or_lease_release; historical_metadata_not_backfilled"
	f.Summary = "Runner execution failed; private error output omitted"
	if f.Provider != "codex" {
		f.Provider = ""
		f.Operation, f.RPCCode, f.ProviderCode, f.MaxChars, f.ActualChars = "", nil, "", nil, nil
	} else {
		f.Summary = "Codex provider request failed; private error output omitted"
	}
	if !slices.Contains([]string{"initialize", "account/read", "config/read", "thread/read", "thread/start", "thread/resume", "turn/start", "turn/steer", "turn/completed", "model/list"}, f.Operation) {
		f.Operation = ""
	}
	if f.ProviderCode != "input_too_large" {
		f.ProviderCode = ""
	}
	if f.MaxChars != nil && *f.MaxChars < 0 {
		f.MaxChars = nil
	}
	if f.ActualChars != nil && *f.ActualChars < 0 {
		f.ActualChars = nil
	}
	if f.Provider == "codex" {
		f.Error = codexPublicError(f.Operation, f.ProviderCode, f.RPCCode)
	}
	f.TurnStartRefused = f.TurnStartRefused && f.Provider == "codex" && f.Operation == "turn/start" && f.RPCCode != nil
	f.Unavailable = []string{}
	if f.Provider == "" {
		f.Unavailable = append(f.Unavailable, "provider")
	}
	if f.Operation == "" {
		f.Unavailable = append(f.Unavailable, "provider_operation")
	}
	if f.RPCCode == nil {
		f.Unavailable = append(f.Unavailable, "rpc_code")
	}
	if f.ProviderCode == "" {
		f.Unavailable = append(f.Unavailable, "provider_code")
	}
	if f.MaxChars == nil {
		f.Unavailable = append(f.Unavailable, "max_chars")
	}
	if f.ActualChars == nil {
		f.Unavailable = append(f.Unavailable, "actual_chars")
	}
	return f
}

func codexPublicError(operation, providerCode string, rpcCode *int) string {
	message := "codex"
	if operation != "" {
		message += " " + strings.ReplaceAll(operation, "/", "_")
	}
	message += " failed"
	switch {
	case providerCode != "":
		message += ": " + providerCode
	case rpcCode != nil:
		message += ": rpc " + strconv.Itoa(*rpcCode)
	}
	return message
}
