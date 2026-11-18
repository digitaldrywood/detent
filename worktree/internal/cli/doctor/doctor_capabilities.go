package doctor

import (
	"errors"
	"fmt"
	"os"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

type CapabilityState string

const (
	CapabilityConfigured  CapabilityState = "configured"
	CapabilityUnused      CapabilityState = "unused"
	capabilityUnavailable CapabilityState = "unavailable"
)

type capability struct {
	Name   string          `json:"name"`
	State  CapabilityState `json:"state"`
	Detail string          `json:"detail"`
}

type CapabilityReport struct {
	ProjectID    string       `json:"project_id"`
	Capabilities []capability `json:"items"`
}

func CheckCapabilities(project globalconfig.Project, workflow workflowconfig.Workflow) Check {
	projectID := ProjectID(project)
	capabilities := projectCapabilities(project, workflow)
	counts := map[CapabilityState]int{}
	for _, capability := range capabilities {
		counts[capability.State]++
	}
	return Check{
		Name:   "Project " + projectID + " capabilities",
		Status: OK,
		Detail: fmt.Sprintf(
			"advisory: %d configured, %d unused, %d unavailable",
			counts[CapabilityConfigured],
			counts[CapabilityUnused],
			counts[capabilityUnavailable],
		),
		Capabilities: &CapabilityReport{
			ProjectID:    projectID,
			Capabilities: capabilities,
		},
	}
}

func projectCapabilities(project globalconfig.Project, workflow workflowconfig.Workflow) []capability {
	cfg := workflow.Config
	return []capability{
		enabledCapability(
			"agent.followups",
			cfg.Agent.Followups.Enabled,
			"enabled; agents receive guidance to file separate backlog issues for meaningful out-of-scope discoveries.",
			"Enable to prompt agents to file separate backlog issues for meaningful out-of-scope discoveries.",
		),
		enabledCapability(
			"agent.auto_promote",
			cfg.Agent.AutoPromote.Enabled,
			"enabled; review-ready work advances automatically after configured gates and quiet windows.",
			"Enable to advance review-ready work automatically after configured gates and quiet windows.",
		),
		releaseCapability(cfg),
		effortRubricCapability(project, cfg),
	}
}

func releaseCapability(cfg workflowconfig.Config) capability {
	if cfg.Release.Enabled {
		return capability{
			Name:   "release",
			State:  CapabilityConfigured,
			Detail: "enabled; Detent cuts releases after configured merge-count, age, and CI gates.",
		}
	}
	if cfg.Tracker.Kind != workflowconfig.TrackerGitHub {
		return capability{
			Name:   "release",
			State:  capabilityUnavailable,
			Detail: "tracker.kind " + cfg.Tracker.Kind + " cannot inspect and tag GitHub repositories; release requires github.",
		}
	}
	return capability{Name: "release", State: CapabilityUnused, Detail: "Enable to cut releases automatically after merge-count, age, and CI gates."}
}

func effortRubricCapability(project globalconfig.Project, cfg workflowconfig.Config) capability {
	root := ProjectSourceRoot(project, cfg)
	if root == "" {
		return capability{Name: "detent-agent effort rubric", State: capabilityUnavailable, Detail: "source root is not configured, so AGENTS.md and CLAUDE.md cannot be inspected."}
	}
	expandedRoot, err := ExpandWorkspacePath(root)
	if err != nil {
		return capability{Name: "detent-agent effort rubric", State: capabilityUnavailable, Detail: "source root could not be resolved: " + err.Error() + "."}
	}
	info, err := os.Stat(expandedRoot)
	if err != nil || !info.IsDir() {
		return capability{Name: "detent-agent effort rubric", State: capabilityUnavailable, Detail: "source repository is unavailable locally, so AGENTS.md and CLAUDE.md cannot be inspected."}
	}
	path, err := FindIssueEffortGuidance(expandedRoot)
	if err != nil {
		return capability{Name: "detent-agent effort rubric", State: capabilityUnavailable, Detail: err.Error() + "."}
	}
	if path != "" {
		return capability{Name: "detent-agent effort rubric", State: CapabilityConfigured, Detail: path + " provides project-specific detent-agent effort guidance."}
	}
	return capability{Name: "detent-agent effort rubric", State: CapabilityUnused, Detail: "Add a project-specific detent-agent effort rubric so issues receive deliberate reasoning effort."}
}

func enabledCapability(name string, enabled bool, configuredDetail string, unusedDetail string) capability {
	if enabled {
		return capability{Name: name, State: CapabilityConfigured, Detail: configuredDetail}
	}
	return capability{Name: name, State: CapabilityUnused, Detail: unusedDetail}
}

func FindIssueEffortGuidance(sourceRoot string) (_ string, resultErr error) {
	root, err := os.OpenRoot(sourceRoot)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("source root could not be opened: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, root.Close())
	}()

	for _, path := range []string{"WORKFLOW.md"} {
		content, err := root.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("%s could not be read: %w", path, err)
		}
		if strings.Contains(strings.ToLower(string(content)), "detent-agent") {
			return path, nil
		}
	}
	return "", nil
}
