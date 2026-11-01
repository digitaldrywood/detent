package config

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

const AgentBackendAntigravity = "antigravity"

type AntigravityOptions struct {
	Home           string `yaml:"home"`
	Effort         string `yaml:"effort"`
	TurnTimeoutMS  int    `yaml:"turn_timeout_ms"`
	StallTimeoutMS int    `yaml:"stall_timeout_ms"`
	Shell          string `yaml:"shell"`
}

func (b AgentBackend) AntigravityOptions() AntigravityOptions {
	options, err := b.decodedAntigravityOptions()
	if err != nil {
		return AntigravityOptions{}
	}
	return options
}

func (b AgentBackend) decodedAntigravityOptions() (AntigravityOptions, error) {
	if b.optionsProblem != "" {
		return AntigravityOptions{}, errors.New(b.optionsProblem)
	}
	var options AntigravityOptions
	if err := b.Options.Decode(&options); err != nil {
		return AntigravityOptions{}, err
	}
	options.Effort = strings.ToLower(strings.TrimSpace(options.Effort))
	options.Shell = strings.TrimSpace(options.Shell)
	options.Home = strings.TrimSpace(options.Home)
	return options, nil
}

func (o AntigravityOptions) validate(prefix string, problems *[]string) {
	if !filepath.IsAbs(o.Home) {
		*problems = append(*problems, prefix+".home must be an absolute path to a dedicated, signed-in Antigravity home")
	}
	if !slices.Contains([]string{"", "low", "medium", "high", "xhigh", "max"}, o.Effort) {
		*problems = append(*problems, prefix+".effort must be one of low, medium, high, xhigh, max")
	}
	if o.TurnTimeoutMS < 0 {
		*problems = append(*problems, prefix+".turn_timeout_ms must be greater than or equal to 0")
	}
	if o.StallTimeoutMS < 0 {
		*problems = append(*problems, prefix+".stall_timeout_ms must be greater than or equal to 0")
	}
}
