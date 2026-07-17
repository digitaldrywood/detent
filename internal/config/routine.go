package config

import (
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"
)

type Routine struct {
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule"`
	Prompt   string `yaml:"prompt"`
}

func NormalizeRoutines(routines []Routine) []Routine {
	out := make([]Routine, len(routines))
	for index, routine := range routines {
		routine.Name = strings.ToLower(strings.TrimSpace(routine.Name))
		routine.Schedule = strings.TrimSpace(routine.Schedule)
		routine.Prompt = strings.TrimSpace(routine.Prompt)
		out[index] = routine
	}
	return out
}

func ValidateRoutines(prefix string, routines []Routine) []string {
	if prefix == "" {
		prefix = "routines"
	}
	routines = NormalizeRoutines(routines)
	problems := []string{}
	seen := map[string]struct{}{}
	for index, routine := range routines {
		field := fmt.Sprintf("%s[%d]", prefix, index)
		if !validAgentIdentityLabel(routine.Name) {
			problems = append(problems, field+".name must be a sanitized label containing only letters, numbers, dots, underscores, or hyphens")
		} else if _, ok := seen[routine.Name]; ok {
			problems = append(problems, field+".name must be unique")
		} else {
			seen[routine.Name] = struct{}{}
		}
		if routine.Schedule == "" {
			problems = append(problems, field+".schedule is required")
		} else if _, err := cron.ParseStandard(routine.Schedule); err != nil {
			problems = append(problems, field+".schedule must be a valid five-field cron expression")
		}
		if routine.Prompt == "" {
			problems = append(problems, field+".prompt is required")
		}
	}
	return problems
}
