package tracker

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

type NativeTerminalFailure struct {
	Rejection        *PublicationRejection `json:"rejection,omitempty"`
	Error            string                `json:"error,omitempty"`
	Stage            string                `json:"stage,omitempty"`
	Attribution      string                `json:"attribution,omitempty"`
	ErrorClass       string                `json:"error_class,omitempty"`
	TurnStartRefused bool                  `json:"turn_start_refused,omitempty"`
	ObservedAt       time.Time             `json:"observed_at"`
	Source           string                `json:"source"`
	Coverage         string                `json:"coverage"`
	Unavailable      []string              `json:"unavailable"`
	Provider         string                `json:"provider,omitempty"`
	Operation        string                `json:"operation,omitempty"`
	RPCCode          *int                  `json:"rpc_code,omitempty"`
	ProviderCode     string                `json:"provider_code,omitempty"`
	MaxChars         *int64                `json:"max_chars,omitempty"`
	ActualChars      *int64                `json:"actual_chars,omitempty"`
	Summary          string                `json:"summary"`
}

var nativeFailureAssignment = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*\s*=\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`)

func (f NativeTerminalFailure) Public() NativeTerminalFailure {
	text := NativeFinalization{}
	first, _, _ := strings.Cut(f.Error, "\n")
	f.Error = text.publicText(nativeFailureAssignment.ReplaceAllString(strings.TrimSpace(first), "[redacted]"))
	switch f.ErrorClass {
	case "backend_startup", "protocol", "workspace_hook", "issue_configuration", "source_recovery", "acceptance_publication", "security_audit_publication", "event_publication", "finalization", "lease_released":
	default:
		f.ErrorClass = ""
	}
	switch f.Stage {
	case "pre_provider", "workspace_preparation", "acceptance_publication", "security_audit_publication", "event_publication", "finalization", "lease_release":
	default:
		f.Stage = ""
	}
	if stage := f.Rejection.TerminalStage(); stage != "" {
		rejection := *f.Rejection
		f.Rejection = &rejection
		f.Error = rejection.DiagnosticError()
		f.Stage, f.ErrorClass = stage, stage
	} else {
		f.Rejection = nil
	}
	f.Attribution = ""
	switch f.ErrorClass {
	case "backend_startup", "protocol", "workspace_hook", "acceptance_publication", "security_audit_publication", "event_publication":
		f.Attribution = "instance"
	case "issue_configuration", "source_recovery":
		f.Attribution = "issue"
	}
	f.Source = "host_runner_completion"
	f.Coverage = "recorded_execution_error_only; not_change_publication_or_issue_acceptance_or_lease_release; historical_metadata_not_backfilled"
	f.Summary = "Runner execution failed; private error output omitted"
	switch f.ErrorClass {
	case "finalization":
		f.Coverage = "recorded_finalization_error_only; historical_metadata_not_backfilled"
		f.Summary = "Change finalization failed; private error output omitted"
	case "lease_released":
		f.Source = "hub_lease_release"
		f.Coverage = "recorded_lease_release_only; historical_metadata_not_backfilled"
		f.Summary = "Attempt lease was released before the attempt finished"
	}
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
	f.TurnStartRefused = f.TurnStartRefused && f.Provider == "codex" && f.Operation == "turn/start" && f.RPCCode != nil
	f.Unavailable = []string{}
	if f.Stage == "" {
		f.Unavailable = append(f.Unavailable, "stage")
	}
	if f.Attribution == "" {
		f.Unavailable = append(f.Unavailable, "attribution")
	}
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
