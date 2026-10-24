package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRetiredConfigurationKeys(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"agent.no_progress_timeout_ms", "agent.no_progress_token_limit", "agent.no_progress_spend_limit_usd",
		"agent.lifetime_session_limit", "agent.lifetime_token_limit", "agent.lifetime_limit_override_label", "agent.lifetime_limit_cooldown_seconds",
		"agent.auto_promote.rework_limit", "agent.auto_promote.no_progress_limit",
		"recovery.terminal_attempt_retry_limit", "tracker.blocked_recovery.breaker_cooldown_seconds", "observability.park_review_threshold",
	} {
		t.Run(path, func(t *testing.T) {
			raw := "0"
			parts := strings.Split(path, ".")
			for i := len(parts) - 1; i >= 0; i-- {
				raw = parts[i] + ": {" + raw + "}"
			}
			raw = strings.Replace(raw, "{0}", "0", 1)
			for _, input := range []string{raw, "defaults: &defaults {" + raw + "}\n<<: *defaults"} {
				cfg := Default()
				err := yaml.Unmarshal([]byte(input), &cfg)
				if err == nil || !strings.Contains(err.Error(), path+" is retired") {
					t.Fatalf("decode error = %v, want %s is retired", err, path)
				}
			}
		})
	}
}
