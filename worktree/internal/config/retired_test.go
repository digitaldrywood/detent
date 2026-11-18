package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRetiredConfigurationKeysAreIgnored(t *testing.T) {
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
				if err := yaml.Unmarshal([]byte(input), &cfg); err != nil {
					t.Fatalf("decode %q: %v", input, err)
				}
				if !reflect.DeepEqual(cfg, Default()) {
					t.Fatalf("decode %q changed configuration", input)
				}
			}
		})
	}
}

func TestRetiredWorkflowKeys(t *testing.T) {
	for _, test := range []struct{ key, yaml string }{
		{"intake", "intake: null\n"},
		{"routines", "routines: []\n"},
		{"retro", "retro: {enabled: false}\n"},
		{"backlog_admission", "backlog_admission: false\n"},
		{"tracker.github_status_source", "tracker: {github_status_source: label}\n"},
		{"tracker.project_slug", "tracker: {project_slug: example}\n"},
		{"tracker.status_field", "tracker: {status_field: Status}\n"},
		{"tracker.status_label_prefix", "tracker: {status_label_prefix: 'detent:'}\n"},
		{"tracker.write_probe_issue", "tracker: {write_probe_issue: 1}\n"},
		{"server.host", "server: {host: localhost}\n"},
		{"server.port", "server: {port: 0}\n"},
		{"server.board_snapshot_stale_after_seconds", "server: {board_snapshot_stale_after_seconds: 90}\n"},
		{"retro", "defaults: &defaults {retro: {enabled: false}}\n<<: *defaults\n"},
	} {
		t.Run(test.key, func(t *testing.T) {
			for _, layout := range []string{"legacy", "structured", "local"} {
				t.Run(layout, func(t *testing.T) {
					var err error
					switch layout {
					case "legacy":
						_, err = ParseWorkflow([]byte("---\n" + test.yaml + "---\nWork.\n"))
					case "structured":
						_, err = ParseProjectDefinition(ProjectDefinitionSources{HasConfig: true, Config: []byte("schema: 1\n" + test.yaml), Workflow: []byte("Work.\n")})
					case "local":
						_, err = ParseProjectDefinition(ProjectDefinitionSources{HasConfig: true, Config: []byte("schema: 1\n"), HasLocalConfig: true, LocalConfig: []byte("schema: 1\n" + test.yaml), Workflow: []byte("Work.\n")})
					}
					if err == nil || !strings.Contains(err.Error(), test.key+": retired configuration key") {
						t.Fatalf("parse error = %v, want retired %s", err, test.key)
					}
				})
			}
		})
	}
}

func TestHistoricalRetiredWorkflowKeys(t *testing.T) {
	for _, layout := range []string{"legacy", "structured"} {
		t.Run(layout, func(t *testing.T) {
			config := "tracker:\n  kind: hub_native\nretro: {enabled: false}\nbacklog_admission: {enabled: false}\ngate:\n  run: make check-landing\n"
			sources := ProjectDefinitionSources{Workflow: []byte("Work.\n")}
			if layout == "legacy" {
				sources.Workflow = []byte("---\n" + config + "---\nWork.\n")
			} else {
				sources.HasConfig = true
				sources.Config = []byte("schema: 1\n" + config)
			}
			historical, err := ParseHistoricalProjectDefinition(sources)
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := ResolvePolicy(historical)
			if err != nil {
				t.Fatal(err)
			}
			applied, err := ApplyNativePolicy(Workflow{Config: Default()}, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if applied.Config.Tracker.Kind != TrackerHubNative || applied.Config.Gate.Run != "make check-landing" || applied.Prompt != "Work." {
				t.Fatalf("historical native policy lost current behavior: tracker=%s gate=%s prompt=%q", applied.Config.Tracker.Kind, applied.Config.Gate.Run, applied.Prompt)
			}
		})
	}
}
