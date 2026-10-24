package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	for _, path := range []string{
		"agent.no_progress_timeout_ms",
		"agent.no_progress_token_limit",
		"agent.no_progress_spend_limit_usd",
		"agent.lifetime_session_limit",
		"agent.lifetime_token_limit",
		"agent.lifetime_limit_override_label",
		"agent.lifetime_limit_cooldown_seconds",
		"agent.auto_promote.rework_limit",
		"agent.auto_promote.no_progress_limit",
		"recovery.terminal_attempt_retry_limit",
		"tracker.blocked_recovery.breaker_cooldown_seconds",
		"observability.park_review_threshold",
	} {
		value := node
		found := true
		for _, key := range strings.Split(path, ".") {
			var values map[string]yaml.Node
			if err := value.Decode(&values); err != nil {
				return err
			}
			nested, ok := values[key]
			if !ok {
				found = false
				break
			}
			value = &nested
		}
		if found {
			return fmt.Errorf("%s is retired; remove this key", path)
		}
	}
	type plain Config
	value := plain(*c)
	if err := node.Decode(&value); err != nil {
		return err
	}
	*c = Config(value)
	return nil
}
