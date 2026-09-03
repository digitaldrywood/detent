package global

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultHubTokenEnvironment     = "DETENT_HUB_TOKEN"
	DefaultHubHeartbeatSeconds     = 30
	DefaultHubLeaseTTLSeconds      = 90
	DefaultHubRequestTimeoutMillis = 10000
)

type HubClient struct {
	URL                      string `yaml:"hub_url,omitempty"`
	TokenEnvironment         string `yaml:"token_env,omitempty"`
	MachineID                string `yaml:"machine_id,omitempty"`
	DisplayName              string `yaml:"display_name,omitempty"`
	Capacity                 int    `yaml:"capacity,omitempty"`
	HeartbeatIntervalSeconds int    `yaml:"heartbeat_interval_seconds,omitempty"`
	LeaseTTLSeconds          int    `yaml:"lease_ttl_seconds,omitempty"`
	RequestTimeoutMS         int    `yaml:"request_timeout_ms,omitempty"`
}

func (c HubClient) IsZero() bool {
	return strings.TrimSpace(c.URL) == "" && strings.TrimSpace(c.TokenEnvironment) == "" &&
		strings.TrimSpace(c.MachineID) == "" && strings.TrimSpace(c.DisplayName) == "" && c.Capacity == 0 &&
		c.HeartbeatIntervalSeconds == 0 && c.LeaseTTLSeconds == 0 && c.RequestTimeoutMS == 0
}

func (c HubClient) Configured() bool {
	return strings.TrimSpace(c.URL) != ""
}

func (c HubClient) Normalized() HubClient {
	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	c.TokenEnvironment = strings.TrimSpace(c.TokenEnvironment)
	if c.TokenEnvironment == "" {
		c.TokenEnvironment = DefaultHubTokenEnvironment
	}
	c.MachineID = strings.TrimSpace(c.MachineID)
	c.DisplayName = strings.TrimSpace(c.DisplayName)
	if c.HeartbeatIntervalSeconds <= 0 {
		c.HeartbeatIntervalSeconds = DefaultHubHeartbeatSeconds
	}
	if c.LeaseTTLSeconds <= 0 {
		c.LeaseTTLSeconds = DefaultHubLeaseTTLSeconds
	}
	if c.RequestTimeoutMS <= 0 {
		c.RequestTimeoutMS = DefaultHubRequestTimeoutMillis
	}
	return c
}

func (c HubClient) HeartbeatInterval() time.Duration {
	return time.Duration(c.Normalized().HeartbeatIntervalSeconds) * time.Second
}

func (c HubClient) LeaseTTL() time.Duration {
	return time.Duration(c.Normalized().LeaseTTLSeconds) * time.Second
}

func (c HubClient) RequestTimeout() time.Duration {
	return time.Duration(c.Normalized().RequestTimeoutMS) * time.Millisecond
}

func (c HubClient) Validate() []string {
	if !c.Configured() {
		if !c.IsZero() {
			return []string{"client.hub_url is required when Hub client settings are configured"}
		}
		return nil
	}
	var problems []string
	parsed, err := url.Parse(strings.TrimSpace(c.URL))
	if err != nil || !parsed.IsAbs() || strings.TrimSpace(parsed.Host) == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		problems = append(problems, "client.hub_url must be an absolute http or https URL")
	}
	if strings.ContainsAny(c.TokenEnvironment, "\r\n") || (strings.TrimSpace(c.TokenEnvironment) != "" && !validEnvironmentName(c.TokenEnvironment)) {
		problems = append(problems, "client.token_env must be an environment variable name")
	}
	if strings.ContainsAny(c.MachineID, "\r\n") {
		problems = append(problems, "client.machine_id must be a single line")
	}
	if c.Capacity < 0 {
		problems = append(problems, "client.capacity must be greater than 0")
	}
	if c.HeartbeatIntervalSeconds < 0 {
		problems = append(problems, "client.heartbeat_interval_seconds must be greater than 0")
	}
	if c.LeaseTTLSeconds < 0 {
		problems = append(problems, "client.lease_ttl_seconds must be greater than 0")
	}
	if c.RequestTimeoutMS < 0 {
		problems = append(problems, "client.request_timeout_ms must be greater than 0")
	}
	normalized := c.Normalized()
	if normalized.HeartbeatIntervalSeconds >= normalized.LeaseTTLSeconds {
		problems = append(problems, "client.heartbeat_interval_seconds must be shorter than client.lease_ttl_seconds")
	}
	return problems
}

func hubClientRawErrors(value any) []string {
	if value == nil {
		return nil
	}
	attrs, ok := value.(map[string]any)
	if !ok {
		return []string{"client: must be a mapping"}
	}
	var problems []string
	for _, name := range []string{"hub_url", "token_env", "machine_id", "display_name"} {
		problems = append(problems, optionalStringTypeError(attrs, name)...)
	}
	for _, field := range []struct{ name, path string }{
		{"capacity", "client.capacity"},
		{"heartbeat_interval_seconds", "client.heartbeat_interval_seconds"},
		{"lease_ttl_seconds", "client.lease_ttl_seconds"},
		{"request_timeout_ms", "client.request_timeout_ms"},
	} {
		if value, configured := attrs[field.name]; configured && !positiveInteger(value) {
			problems = append(problems, field.path+": must be a positive integer")
		}
	}
	return problems
}

func buildHubClient(value any) (HubClient, error) {
	if value == nil {
		return HubClient{}, nil
	}
	if _, err := mapValue(value, "client"); err != nil {
		return HubClient{}, err
	}
	var client HubClient
	if err := decodeYAMLValue(value, &client); err != nil {
		return HubClient{}, fmt.Errorf("client: %w", err)
	}
	return client, nil
}

func validEnvironmentName(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for index, char := range value {
		if (char >= 'A' && char <= 'Z') || char == '_' || (index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}
