package runnerauth

import (
	"encoding/json"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/workspacesession"

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
	Report      *MachineReport    `json:"report,omitempty"`
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
	ReportReceipt *RoutingSnapshot                  `json:"report_receipt,omitempty"`
	Lease         *tracker.NativeLease              `json:"lease,omitempty"`
	LeaseProject  tracker.ProjectID                 `json:"lease_project,omitempty"`
	NoChange      bool                              `json:"no_change"`
	Versions      *SyncVersions                     `json:"versions,omitempty"`
	Routing       *RoutingSnapshot                  `json:"routing,omitempty"`
	Projects      map[tracker.ProjectID]SyncProject `json:"projects,omitempty"`
}

type MachineReport struct {
	HostMetrics           json.RawMessage                     `json:"host_metrics,omitempty"`
	ChangeCursor          *string                             `json:"change_cursor,omitempty"`
	Admission             *tracker.NativeAdmissionObservation `json:"admission,omitempty"`
	SpriteName            string                              `json:"sprite_name,omitempty"`
	Update                *UpdateObservation                  `json:"update,omitempty"`
	CapacityConfig        *CapacityConfig                     `json:"capacity_configuration,omitempty"`
	ProjectConfiguration  *ProjectConfiguration               `json:"project_configuration,omitempty"`
	LocalChecks           *LocalChecks                        `json:"local_checks,omitempty"`
	Problems              []Problem                           `json:"problems"`
	ProtocolMajor         int                                 `json:"protocol_major,omitempty"`
	SettingsRejected      bool                                `json:"settings_rejected,omitempty"`
	BackendIsolation      isolation.Report                    `json:"backend_isolation,omitempty"`
	ProviderReports       []providercapacity.Report           `json:"provider_reports,omitempty"`
	DisplayName           string                              `json:"display_name"`
	Capacity              int                                 `json:"capacity"`
	Version               string                              `json:"version"`
	OS                    string                              `json:"os,omitempty"`
	Architecture          string                              `json:"architecture,omitempty"`
	WorkspaceCapabilities *workspacesession.Capabilities      `json:"workspace_capabilities,omitempty"`
	WorkspaceIsolation    string                              `json:"workspace_isolation,omitempty"`
	Skills                *[]skills.ProviderSkill             `json:"skills,omitempty"`
	CheckoutRepository    *string                             `json:"checkout_repository,omitempty"`
}
