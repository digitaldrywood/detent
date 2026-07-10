package intake

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/robfig/cron/v3"
)

var sourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

const (
	KindWebhook  = "webhook"
	KindSentry   = "sentry"
	KindDatadog  = "datadog"
	KindSlack    = "slack"
	KindSchedule = "schedule"

	defaultStatus   = "Backlog"
	defaultTitle    = "[{source}] {summary}"
	defaultBody     = "{details}"
	defaultDedupeBy = "fingerprint"
)

type Config struct {
	Sources []Source `yaml:"sources,omitempty"`
}

type Source struct {
	Name     string  `yaml:"name,omitempty"`
	Kind     string  `yaml:"kind"`
	Secret   string  `yaml:"secret,omitempty"`
	Match    string  `yaml:"match,omitempty"`
	Cron     string  `yaml:"cron,omitempty"`
	Scan     string  `yaml:"scan,omitempty"`
	Creates  Creates `yaml:"creates"`
	DedupeBy string  `yaml:"dedupe_by,omitempty"`
}

type Creates struct {
	Status string   `yaml:"status"`
	Labels []string `yaml:"labels,omitempty"`
	Title  string   `yaml:"title"`
	Body   string   `yaml:"body,omitempty"`
}

func (c Config) Enabled() bool {
	return len(c.Sources) > 0
}

func (c *Config) Normalize() {
	if c == nil {
		return
	}
	for index := range c.Sources {
		source := &c.Sources[index]
		source.Kind = strings.ToLower(strings.TrimSpace(source.Kind))
		source.Name = strings.ToLower(strings.TrimSpace(source.Name))
		source.Secret = strings.TrimSpace(source.Secret)
		source.Match = strings.TrimSpace(source.Match)
		source.Cron = strings.TrimSpace(source.Cron)
		source.Scan = strings.ToLower(strings.TrimSpace(source.Scan))
		source.DedupeBy = strings.TrimSpace(source.DedupeBy)
		if source.DedupeBy == "" {
			source.DedupeBy = defaultDedupeBy
		}
		if source.Name == "" {
			source.Name = source.Kind
			if source.Kind == KindSchedule && source.Scan != "" {
				source.Name = source.Scan
			}
		}
		source.Creates.Status = strings.TrimSpace(source.Creates.Status)
		if source.Creates.Status == "" {
			source.Creates.Status = defaultStatus
		}
		source.Creates.Title = strings.TrimSpace(source.Creates.Title)
		if source.Creates.Title == "" {
			source.Creates.Title = defaultTitle
		}
		source.Creates.Body = strings.TrimSpace(source.Creates.Body)
		if source.Creates.Body == "" {
			source.Creates.Body = defaultBody
		}
		source.Creates.Labels = normalizeLabels(source.Creates.Labels)
	}
}

func (c Config) Validate(prefix string, states []string) []string {
	c.Normalize()
	if prefix == "" {
		prefix = "intake"
	}

	problems := []string{}
	seen := map[string]struct{}{}
	for index, source := range c.Sources {
		field := fmt.Sprintf("%s.sources[%d]", prefix, index)
		if source.Name == "" {
			problems = append(problems, field+".name is required")
		} else if !sourceNamePattern.MatchString(source.Name) {
			problems = append(problems, field+".name must contain only lowercase letters, numbers, dots, underscores, or hyphens")
		} else if _, ok := seen[source.Name]; ok {
			problems = append(problems, field+".name must be unique")
		} else {
			seen[source.Name] = struct{}{}
		}

		switch source.Kind {
		case KindSchedule:
			if source.Cron == "" {
				problems = append(problems, field+".cron is required for scheduled sources")
			} else if _, err := cron.ParseStandard(source.Cron); err != nil {
				problems = append(problems, field+".cron must be a valid five-field cron expression")
			}
			if source.Scan == "" {
				problems = append(problems, field+".scan is required for scheduled sources")
			}
		default:
			if source.Kind == "" {
				problems = append(problems, field+".kind is required")
			}
			if source.Secret == "" {
				problems = append(problems, field+".secret is required for webhook sources")
			}
		}
		if strings.ContainsAny(source.Secret, "\r\n") {
			problems = append(problems, field+".secret must be a single line")
		}

		if source.Match != "" {
			key, value, ok := strings.Cut(source.Match, ":")
			if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
				problems = append(problems, field+".match must use field:value syntax")
			}
		}
		if source.DedupeBy == "" {
			problems = append(problems, field+".dedupe_by is required")
		}
		if source.Creates.Status == "" {
			problems = append(problems, field+".creates.status is required")
		} else if len(states) > 0 && !containsFold(states, source.Creates.Status) {
			problems = append(problems, field+".creates.status must name a configured tracker state")
		}
		if source.Creates.Title == "" {
			problems = append(problems, field+".creates.title is required")
		}
	}
	return problems
}

func cloneConfig(cfg Config) Config {
	out := Config{Sources: make([]Source, len(cfg.Sources))}
	copy(out.Sources, cfg.Sources)
	for index := range out.Sources {
		out.Sources[index].Creates.Labels = append([]string(nil), cfg.Sources[index].Creates.Labels...)
	}
	return out
}

func normalizeLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	seen := map[string]struct{}{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		key := strings.ToLower(label)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, label)
	}
	return out
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}
