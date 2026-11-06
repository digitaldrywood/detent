package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

type doctorCapabilityState string

const (
	doctorCapabilityConfigured  doctorCapabilityState = "configured"
	doctorCapabilityUnused      doctorCapabilityState = "unused"
	doctorCapabilityUnavailable doctorCapabilityState = "unavailable"
)

type doctorCapability struct {
	Name   string                `json:"name"`
	State  doctorCapabilityState `json:"state"`
	Detail string                `json:"detail"`
}

type doctorCapabilityReport struct {
	ProjectID    string             `json:"project_id"`
	Capabilities []doctorCapability `json:"items"`
}

func checkDoctorCapabilities(project globalconfig.Project, workflow workflowconfig.Workflow) doctorCheck {
	projectID := doctorProjectID(project)
	capabilities := doctorProjectCapabilities(project, workflow)
	counts := map[doctorCapabilityState]int{}
	for _, capability := range capabilities {
		counts[capability.State]++
	}
	return doctorCheck{
		Name:   "Project " + projectID + " capabilities",
		Status: doctorOK,
		Detail: fmt.Sprintf(
			"advisory: %d configured, %d unused, %d unavailable",
			counts[doctorCapabilityConfigured],
			counts[doctorCapabilityUnused],
			counts[doctorCapabilityUnavailable],
		),
		Capabilities: &doctorCapabilityReport{
			ProjectID:    projectID,
			Capabilities: capabilities,
		},
	}
}

func doctorProjectCapabilities(project globalconfig.Project, workflow workflowconfig.Workflow) []doctorCapability {
	cfg := workflow.Config
	return []doctorCapability{
		doctorEnabledCapability(
			"agent.followups",
			cfg.Agent.Followups.Enabled,
			"enabled; agents receive guidance to file separate backlog issues for meaningful out-of-scope discoveries.",
			"Enable to prompt agents to file separate backlog issues for meaningful out-of-scope discoveries.",
		),
		doctorEnabledCapability(
			"agent.auto_promote",
			cfg.Agent.AutoPromote.Enabled,
			"enabled; review-ready work advances automatically after configured gates and quiet windows.",
			"Enable to advance review-ready work automatically after configured gates and quiet windows.",
		),
		doctorReleaseCapability(cfg),
		doctorEffortRubricCapability(project, cfg),
	}
}

func doctorReleaseCapability(cfg workflowconfig.Config) doctorCapability {
	if cfg.Release.Enabled {
		return doctorCapability{
			Name:   "release",
			State:  doctorCapabilityConfigured,
			Detail: "enabled; Detent cuts releases after configured merge-count, age, and CI gates.",
		}
	}
	if cfg.Tracker.Kind != workflowconfig.TrackerGitHub {
		return doctorCapability{
			Name:   "release",
			State:  doctorCapabilityUnavailable,
			Detail: "tracker.kind " + cfg.Tracker.Kind + " cannot inspect and tag GitHub repositories; release requires github.",
		}
	}
	return doctorCapability{Name: "release", State: doctorCapabilityUnused, Detail: "Enable to cut releases automatically after merge-count, age, and CI gates."}
}

func doctorEffortRubricCapability(project globalconfig.Project, cfg workflowconfig.Config) doctorCapability {
	root := projectSourceRoot(project, cfg)
	if root == "" {
		return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityUnavailable, Detail: "source root is not configured, so AGENTS.md and CLAUDE.md cannot be inspected."}
	}
	expandedRoot, err := expandDoctorWorkspacePath(root)
	if err != nil {
		return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityUnavailable, Detail: "source root could not be resolved: " + err.Error() + "."}
	}
	info, err := os.Stat(expandedRoot)
	if err != nil || !info.IsDir() {
		return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityUnavailable, Detail: "source repository is unavailable locally, so AGENTS.md and CLAUDE.md cannot be inspected."}
	}
	path, err := findDoctorIssueEffortGuidance(expandedRoot)
	if err != nil {
		return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityUnavailable, Detail: err.Error() + "."}
	}
	if path != "" {
		return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityConfigured, Detail: path + " provides project-specific detent-agent effort guidance."}
	}
	return doctorCapability{Name: "detent-agent effort rubric", State: doctorCapabilityUnused, Detail: "Add a project-specific detent-agent effort rubric so issues receive deliberate reasoning effort."}
}

func doctorEnabledCapability(name string, enabled bool, configuredDetail string, unusedDetail string) doctorCapability {
	if enabled {
		return doctorCapability{Name: name, State: doctorCapabilityConfigured, Detail: configuredDetail}
	}
	return doctorCapability{Name: name, State: doctorCapabilityUnused, Detail: unusedDetail}
}

func findDoctorIssueEffortGuidance(sourceRoot string) (_ string, resultErr error) {
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
