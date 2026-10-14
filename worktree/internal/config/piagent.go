package config

import (
	"errors"
	"slices"
	"strings"
)

const AgentBackendPiAgent = "pi_agent"

// PiAgentOptions contains only CLI/RPC options supported by Pi. Provider and
// model selection use the backend provider and existing route model fields.
type PiAgentOptions struct {
	ThinkingLevel  string   `yaml:"thinking_level"`
	Tools          []string `yaml:"tools"`
	SessionDir     string   `yaml:"session_dir"`
	TurnTimeoutMS  int      `yaml:"turn_timeout_ms"`
	StallTimeoutMS int      `yaml:"stall_timeout_ms"`
	Shell          string   `yaml:"shell"`
}

func (b AgentBackend) PiAgentOptions() PiAgentOptions {
	options, err := b.decodedPiAgentOptions()
	if err != nil {
		return PiAgentOptions{}
	} // Validation reports decoding failures.
	return options
}

func (b AgentBackend) decodedPiAgentOptions() (PiAgentOptions, error) {
	if b.optionsProblem != "" {
		return PiAgentOptions{}, errors.New(b.optionsProblem)
	}
	var options PiAgentOptions
	if err := b.Options.Decode(&options); err != nil {
		return PiAgentOptions{}, err
	}
	options.ThinkingLevel = strings.ToLower(strings.TrimSpace(options.ThinkingLevel))
	options.SessionDir = strings.TrimSpace(options.SessionDir)
	options.Shell = strings.TrimSpace(options.Shell)
	if len(options.Tools) == 0 {
		options.Tools = []string{"read", "bash", "edit", "write", "grep", "find", "ls"}
	}
	return options, nil
}

func (o PiAgentOptions) validate(prefix string, problems *[]string) {
	if !slices.Contains([]string{"", "off", "minimal", "low", "medium", "high", "xhigh", "max"}, o.ThinkingLevel) {
		*problems = append(*problems, prefix+".thinking_level must be one of off, minimal, low, medium, high, xhigh, max")
	}
	for _, tool := range o.Tools {
		if !slices.Contains([]string{"read", "bash", "edit", "write", "grep", "find", "ls"}, tool) {
			*problems = append(*problems, prefix+".tools must contain only Pi built-in tools: read, bash, edit, write, grep, find, ls")
			break
		}
	}
	if o.TurnTimeoutMS < 0 {
		*problems = append(*problems, prefix+".turn_timeout_ms must be greater than or equal to 0")
	}
	if o.StallTimeoutMS < 0 {
		*problems = append(*problems, prefix+".stall_timeout_ms must be greater than or equal to 0")
	}
}
