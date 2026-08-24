package scheduleowner

import (
	"strings"
	"time"
)

const (
	BackendGitHubRef           = "github_ref"
	DefaultBranch              = "detent-schedule-coordination"
	DefaultLeaseSeconds        = 300
	DefaultHeartbeatSeconds    = 60
	DefaultRetrySeconds        = 15
	DefaultMaxClockSkewSeconds = 15
)

type Config struct {
	Enabled             bool   `yaml:"enabled"`
	Backend             string `yaml:"backend"`
	Key                 string `yaml:"key"`
	Endpoint            string `yaml:"endpoint,omitempty"`
	Repository          string `yaml:"repository"`
	Branch              string `yaml:"branch"`
	LeaseSeconds        int    `yaml:"lease_seconds"`
	HeartbeatSeconds    int    `yaml:"heartbeat_seconds"`
	RetrySeconds        int    `yaml:"retry_seconds"`
	MaxClockSkewSeconds int    `yaml:"max_clock_skew_seconds"`
}

func (c Config) Normalized(defaultRepository string, defaultEndpoint string) Config {
	c.Backend = strings.ToLower(strings.TrimSpace(c.Backend))
	if c.Backend == "" {
		c.Backend = BackendGitHubRef
	}
	c.Key = strings.TrimSpace(c.Key)
	c.Endpoint = strings.TrimSpace(c.Endpoint)
	if c.Endpoint == "" {
		c.Endpoint = strings.TrimSpace(defaultEndpoint)
	}
	c.Repository = strings.TrimSpace(c.Repository)
	if c.Repository == "" {
		c.Repository = strings.TrimSpace(defaultRepository)
	}
	c.Branch = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Branch), "refs/heads/"))
	if c.Branch == "" {
		c.Branch = DefaultBranch
	}
	if c.LeaseSeconds == 0 {
		c.LeaseSeconds = DefaultLeaseSeconds
	}
	if c.HeartbeatSeconds == 0 {
		c.HeartbeatSeconds = DefaultHeartbeatSeconds
	}
	if c.RetrySeconds == 0 {
		c.RetrySeconds = DefaultRetrySeconds
	}
	if c.MaxClockSkewSeconds == 0 {
		c.MaxClockSkewSeconds = DefaultMaxClockSkewSeconds
	}
	return c
}

func (c Config) Validate(prefix string) []string {
	if !c.Enabled {
		return nil
	}
	if prefix == "" {
		prefix = "schedule_ownership"
	}
	problems := []string{}
	if c.Backend != BackendGitHubRef {
		problems = append(problems, prefix+".backend must be github_ref")
	}
	if c.Key == "" {
		problems = append(problems, prefix+".key is required when enabled is true")
	} else if strings.ContainsAny(c.Key, "\r\n") {
		problems = append(problems, prefix+".key must be a single line")
	}
	if !validRepository(c.Repository) {
		problems = append(problems, prefix+".repository must use owner/name syntax")
	}
	if !validBranch(c.Branch) {
		problems = append(problems, prefix+".branch must be a valid branch name")
	}
	if c.LeaseSeconds <= 0 {
		problems = append(problems, prefix+".lease_seconds must be greater than zero")
	}
	if c.HeartbeatSeconds <= 0 {
		problems = append(problems, prefix+".heartbeat_seconds must be greater than zero")
	}
	if c.RetrySeconds <= 0 {
		problems = append(problems, prefix+".retry_seconds must be greater than zero")
	}
	if c.MaxClockSkewSeconds < 0 {
		problems = append(problems, prefix+".max_clock_skew_seconds must not be negative")
	}
	if c.MaxClockSkewSeconds*2 >= c.LeaseSeconds {
		problems = append(problems, prefix+".max_clock_skew_seconds must be less than half lease_seconds")
	}
	if c.HeartbeatSeconds >= c.LeaseSeconds-c.MaxClockSkewSeconds*2 {
		problems = append(problems, prefix+".heartbeat_seconds must leave more than twice max_clock_skew_seconds before lease expiry")
	}
	return problems
}

func (c Config) LeaseTTL() time.Duration {
	return time.Duration(c.LeaseSeconds) * time.Second
}

func (c Config) HeartbeatInterval() time.Duration {
	return time.Duration(c.HeartbeatSeconds) * time.Second
}

func (c Config) RetryInterval() time.Duration {
	return time.Duration(c.RetrySeconds) * time.Second
}

func (c Config) MaxClockSkew() time.Duration {
	return time.Duration(c.MaxClockSkewSeconds) * time.Second
}

func validRepository(repository string) bool {
	owner, name, ok := strings.Cut(strings.TrimSpace(repository), "/")
	return ok && strings.TrimSpace(owner) != "" && strings.TrimSpace(name) != "" && !strings.Contains(name, "/")
}

func validBranch(branch string) bool {
	branch = strings.TrimSpace(branch)
	return branch != "" &&
		!strings.HasPrefix(branch, "/") &&
		!strings.HasSuffix(branch, "/") &&
		!strings.HasSuffix(branch, ".") &&
		!strings.Contains(branch, "..") &&
		!strings.Contains(branch, "@{") &&
		!strings.ContainsAny(branch, " ~^:?*[\\")
}
