package doctor

import (
	"context"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

const commandTimeout = 5 * time.Second

type Status string

const (
	OK   Status = "OK"
	Warn Status = "WARN"
	Fail Status = "FAIL"
)

type Check struct {
	Name                     string                     `json:"name"`
	Status                   Status                     `json:"status"`
	Detail                   string                     `json:"detail"`
	Hint                     string                     `json:"hint,omitempty"`
	WorkflowSkillSuggestions []WorkflowSkillSuggestion  `json:"workflow_skill_suggestions,omitempty"`
	Capabilities             *CapabilityReport          `json:"capabilities,omitempty"`
	WorkflowOptimization     WorkflowOptimizationReport `json:"-"`
}

type Deps struct {
	LookupEnv            func(string) string
	ResolveCommandInDir  func(context.Context, string, []string, string) (string, error)
	RunCommandInDir      func(context.Context, string, []string, string, ...string) error
	ShipSkillProbe       func(string) (ShipSkill, error)
	GitHubRepositoryInfo func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryInfo, error)
	GitWorktrees         func(context.Context, string) ([]GitWorktree, error)
}
