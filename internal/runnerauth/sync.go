package runnerauth

import (
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SyncProjectVersion struct {
	Queued        bool   `json:"queued,omitempty"`
	Policy        string `json:"policy,omitempty"`
	Configuration string `json:"configuration,omitempty"`
	Sequence      int64  `json:"sequence,omitempty"`
}

type SyncVersions struct {
	Routing  int64                                    `json:"routing"`
	Cursor   string                                   `json:"cursor"`
	Projects map[tracker.ProjectID]SyncProjectVersion `json:"projects"`
}

type SyncClaim struct {
	SessionID  string                                    `json:"session_id"`
	TTLSeconds int64                                     `json:"ttl_seconds"`
	Projects   map[tracker.ProjectID]tracker.NativeClaim `json:"projects"`
}

type SyncRequest struct {
	Claim       *SyncClaim        `json:"claim,omitempty"`
	MachineID   tracker.MachineID `json:"machine_id"`
	Versions    SyncVersions      `json:"versions"`
	WaitSeconds int               `json:"wait_seconds,omitempty"`
}

type SyncProject struct {
	Policy        *policy.Approval      `json:"policy,omitempty"`
	Configuration *ProjectConfiguration `json:"configuration,omitempty"`
}

type SyncResponse struct {
	Lease        *tracker.NativeLease              `json:"lease,omitempty"`
	LeaseProject tracker.ProjectID                 `json:"lease_project,omitempty"`
	NoChange     bool                              `json:"no_change"`
	Versions     *SyncVersions                     `json:"versions,omitempty"`
	Routing      *RoutingSnapshot                  `json:"routing,omitempty"`
	Projects     map[tracker.ProjectID]SyncProject `json:"projects,omitempty"`
}
