package runnerauth

import (
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	MaxEnrollmentTTL = 15 * time.Minute
	CredentialTTL    = 24 * time.Hour
	Read             = "read"
	Collaborate      = "collaborate"
	Claim            = "claim"
	Heartbeat        = "heartbeat"
	Events           = "events"
)

type Binding struct {
	RunnerID  string            `json:"runner_id"`
	MachineID tracker.MachineID `json:"machine_id"`
}

type EnrollmentRequest struct {
	Binding
	SharedMachine bool                `json:"shared_machine,omitempty"`
	ProjectIDs    []tracker.ProjectID `json:"project_ids"`
	Operations    []string            `json:"operations"`
	TTLSeconds    int64               `json:"ttl_seconds"`
}

type Enrollment struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Redemption struct {
	Binding
	Credential   string `json:"credential"`
	Hostname     string `json:"hostname"`
	DisplayName  string `json:"display_name"`
	Capacity     int    `json:"capacity"`
	Version      string `json:"version"`
	OS           string `json:"os,omitempty"`
	Architecture string `json:"architecture,omitempty"`
}

type Identity struct {
	Binding
	OrganizationID tracker.OrganizationID `json:"organization_id"`
	ProjectIDs     []tracker.ProjectID    `json:"project_ids"`
	Operations     []string               `json:"operations"`
	ExpiresAt      time.Time              `json:"expires_at"`
}

type Rotation struct {
	Credential string `json:"credential"`
}

func NewBinding() Binding {
	return Binding{RunnerID: newID("runner"), MachineID: tracker.MachineID(newID("machine"))}
}

func newID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func (b Binding) Valid() bool {
	return validID(b.RunnerID, "runner") && validID(string(b.MachineID), "machine")
}

func validID(id, prefix string) bool {
	value, ok := strings.CutPrefix(id, prefix+"_")
	if !ok || len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ValidOperations(operations []string) bool {
	if len(operations) == 0 || len(operations) > 5 {
		return false
	}
	for i, operation := range operations {
		if !slices.Contains([]string{Read, Collaborate, Claim, Heartbeat, Events}, operation) || slices.Contains(operations[:i], operation) {
			return false
		}
	}
	return true
}

func ValidCredential(value string) bool {
	return strings.TrimSpace(value) == value && apikey.ValidateTokenFormat(value) == nil
}
