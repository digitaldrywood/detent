package config

import (
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

const (
	DefaultRoutineMaxFindingsPerRun = 3
	DefaultRoutineMaxOpenFindings   = 10
)

type Routine struct {
	Name                  string `yaml:"name"`
	Schedule              string `yaml:"schedule"`
	Prompt                string `yaml:"prompt"`
	MaxFindingsPerRun     int    `yaml:"max_findings_per_run,omitempty"`
	MaxOpenFindings       int    `yaml:"max_open_findings,omitempty"`
	maxFindingsConfigured bool
	maxOpenConfigured     bool
}

func (r *Routine) UnmarshalYAML(node *yaml.Node) error {
	var decoded struct {
		Name              string `yaml:"name"`
		Schedule          string `yaml:"schedule"`
		Prompt            string `yaml:"prompt"`
		MaxFindingsPerRun *int   `yaml:"max_findings_per_run"`
		MaxOpenFindings   *int   `yaml:"max_open_findings"`
	}
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*r = Routine{
		Name:                  decoded.Name,
		Schedule:              decoded.Schedule,
		Prompt:                decoded.Prompt,
		MaxFindingsPerRun:     DefaultRoutineMaxFindingsPerRun,
		MaxOpenFindings:       DefaultRoutineMaxOpenFindings,
		maxFindingsConfigured: decoded.MaxFindingsPerRun != nil,
		maxOpenConfigured:     decoded.MaxOpenFindings != nil,
	}
	if decoded.MaxFindingsPerRun != nil {
		r.MaxFindingsPerRun = *decoded.MaxFindingsPerRun
	}
	if decoded.MaxOpenFindings != nil {
		r.MaxOpenFindings = *decoded.MaxOpenFindings
	}
	return nil
}

func NormalizeRoutines(routines []Routine) []Routine {
	out := make([]Routine, len(routines))
	for index, routine := range routines {
		routine.Name = strings.ToLower(strings.TrimSpace(routine.Name))
		routine.Schedule = strings.TrimSpace(routine.Schedule)
		routine.Prompt = strings.TrimSpace(routine.Prompt)
		if routine.MaxFindingsPerRun == 0 && !routine.maxFindingsConfigured {
			routine.MaxFindingsPerRun = DefaultRoutineMaxFindingsPerRun
		}
		if routine.MaxOpenFindings == 0 && !routine.maxOpenConfigured {
			routine.MaxOpenFindings = DefaultRoutineMaxOpenFindings
		}
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
		validatePositive(field+".max_findings_per_run", routine.MaxFindingsPerRun, &problems)
		validatePositive(field+".max_open_findings", routine.MaxOpenFindings, &problems)
	}
	return problems
}
