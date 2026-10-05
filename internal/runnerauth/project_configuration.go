package runnerauth

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
)

type ProjectConfigurationRequest struct {
	RequestID              string `json:"request_id,omitempty"`
	Operation              string `json:"operation,omitempty"`
	AllowLocalBinding      *bool  `json:"allow_local_binding,omitempty"`
	ProjectID              string `json:"project_id"`
	ExpectedConfigRevision string `json:"expected_config_revision,omitempty"`
	ExpectedPolicyID       string `json:"expected_policy_id,omitempty"`
	PolicyID               string `json:"policy_id,omitempty"`
	SourceRevision         string `json:"source_revision,omitempty"`
	Checkpoint             string `json:"checkpoint,omitempty"`
}

type ProjectConfigurationCommand struct {
	Request        ProjectConfigurationRequest `json:"request"`
	Issuer         json.RawMessage             `json:"issuer"`
	RunnerRevision int64                       `json:"runner_revision"`
}

type ProjectConfiguration struct {
	LocalIntakeEnabled      bool               `json:"local_intake_enabled"`
	LocalIntakeRemaining    []string           `json:"local_intake_remaining"`
	LocalIntakeBlocked      []string           `json:"local_intake_blocked"`
	RunnerID                string             `json:"runner_id,omitempty"`
	RunnerRevision          int64              `json:"runner_revision,omitempty"`
	RequestID               string             `json:"request_id,omitempty"`
	AllowLocalBinding       bool               `json:"allow_local_binding"`
	LocalBindingPolicy      *policy.Descriptor `json:"local_binding_policy,omitempty"`
	RestrictedBindingPolicy *policy.Descriptor `json:"restricted_binding_policy,omitempty"`
	ProjectID               string             `json:"project_id"`
	Authority               string             `json:"authority"`
	ConfigRevision          string             `json:"config_revision,omitempty"`
	Registered              bool               `json:"registered"`
	RuntimeRegistered       bool               `json:"runtime_registered"`
	Paused                  bool               `json:"paused"`
	Draining                bool               `json:"draining"`
	UnsettledAttempts       int                `json:"unsettled_attempts"`
	Source                  string             `json:"source"`
	SelectedPolicy          *policy.Descriptor `json:"selected_policy,omitempty"`
	EffectivePolicy         *policy.Descriptor `json:"effective_policy,omitempty"`
	Pending                 bool               `json:"pending,omitempty"`
	Saved                   bool               `json:"saved"`
	Applied                 bool               `json:"applied"`
	Constraint              string             `json:"constraint,omitempty"`
	ObservedAt              time.Time          `json:"observed_at"`
}

func (c ProjectConfiguration) Validate() error {
	if c.ProjectID == "" || len(c.ProjectID) > 256 || len(c.RequestID) > 128 || c.Authority != "local_global_configuration" || c.UnsettledAttempts < 0 || c.ObservedAt.IsZero() || len(c.Constraint) > 1120 {
		return errors.New("invalid project configuration evidence")
	}
	if c.ConfigRevision != "" {
		raw, err := hex.DecodeString(c.ConfigRevision)
		if err != nil || len(raw) != 32 {
			return errors.New("invalid project configuration revision")
		}
	}
	switch c.Source {
	case "unavailable", "configured_committed_workflow", "configured_local_workflow_read_only":
	default:
		return errors.New("invalid project configuration source")
	}
	for _, descriptor := range []*policy.Descriptor{c.SelectedPolicy, c.EffectivePolicy, c.LocalBindingPolicy, c.RestrictedBindingPolicy} {
		if descriptor != nil {
			if err := descriptor.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}
