package intake

import (
	"strings"
	"testing"
)

func TestConfigNormalizeAppliesSafeDefaults(t *testing.T) {
	t.Parallel()

	cfg := Config{Sources: []Source{{
		Kind:   " WebHook ",
		Secret: " $INTAKE_SECRET ",
		Creates: Creates{
			Labels: []string{"bug", "Bug", " "},
		},
	}}}
	cfg.Normalize()

	source := cfg.Sources[0]
	if source.Name != KindWebhook || source.Kind != KindWebhook {
		t.Fatalf("source identity = %q/%q, want webhook/webhook", source.Name, source.Kind)
	}
	if source.DedupeBy != "fingerprint" || source.Creates.Status != "Backlog" {
		t.Fatalf("source defaults = %#v", source)
	}
	if source.Creates.Title != "[{source}] {summary}" || source.Creates.Body != "{details}" {
		t.Fatalf("create templates = %#v", source.Creates)
	}
	if len(source.Creates.Labels) != 1 || source.Creates.Labels[0] != "bug" {
		t.Fatalf("labels = %#v, want [bug]", source.Creates.Labels)
	}
}

func TestConfigValidateRejectsInvalidSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{
			name:   "webhook secret",
			config: Config{Sources: []Source{{Kind: KindWebhook}}},
			want:   "secret is required",
		},
		{
			name:   "source name",
			config: Config{Sources: []Source{{Name: "invalid/name", Kind: KindWebhook, Secret: "secret"}}},
			want:   "name must contain only",
		},
		{
			name:   "cron expression",
			config: Config{Sources: []Source{{Kind: KindSchedule, Cron: "not cron", Scan: "stale-todos"}}},
			want:   "valid five-field cron",
		},
		{
			name: "configured status",
			config: Config{Sources: []Source{{
				Kind:    KindWebhook,
				Secret:  "secret",
				Creates: Creates{Status: "Unknown"},
			}}},
			want: "configured tracker state",
		},
		{
			name: "duplicate name",
			config: Config{Sources: []Source{
				{Kind: KindWebhook, Name: "alerts", Secret: "secret"},
				{Kind: KindSlack, Name: "alerts", Secret: "secret"},
			}},
			want: "name must be unique",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			problems := tt.config.Validate("intake", []string{"Backlog", "Todo"})
			if !strings.Contains(strings.Join(problems, "; "), tt.want) {
				t.Fatalf("Validate() = %#v, want %q", problems, tt.want)
			}
		})
	}
}

func TestEmptyConfigIsDisabledAndValid(t *testing.T) {
	t.Parallel()

	cfg := Config{}
	if cfg.Enabled() {
		t.Fatal("Enabled() = true, want false")
	}
	if problems := cfg.Validate("intake", []string{"Backlog"}); len(problems) != 0 {
		t.Fatalf("Validate() = %#v, want no problems", problems)
	}
}
