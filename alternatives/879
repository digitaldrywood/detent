package config

import "errors"

const AgentBackendOpenCode = "opencode"

type OpenCodeOptions struct {
	TurnTimeoutMS  int    `yaml:"turn_timeout_ms"`
	StallTimeoutMS int    `yaml:"stall_timeout_ms"`
	Shell          string `yaml:"shell"`
}

func (b AgentBackend) OpenCodeOptions() OpenCodeOptions {
	options, err := b.decodedOpenCodeOptions()
	if err != nil {
		return OpenCodeOptions{}
	}
	return options
}
func (b AgentBackend) decodedOpenCodeOptions() (OpenCodeOptions, error) {
	if b.optionsProblem != "" {
		return OpenCodeOptions{}, errors.New(b.optionsProblem)
	}
	var options OpenCodeOptions
	err := b.Options.Decode(&options)
	return options, err
}
func (o OpenCodeOptions) validate(prefix string, problems *[]string) {
	if o.TurnTimeoutMS < 0 {
		*problems = append(*problems, prefix+".turn_timeout_ms must be greater than or equal to 0")
	}
	if o.StallTimeoutMS < 0 {
		*problems = append(*problems, prefix+".stall_timeout_ms must be greater than or equal to 0")
	}
}
