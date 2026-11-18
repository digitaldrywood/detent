package workspacerelay

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type TerminalRecording struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	StreamID       string     `json:"stream_id"`
	RelaySessionID string     `json:"relay_session_id"`
	PrincipalID    string     `json:"principal_id"`
	Subject        string     `json:"subject"`
	Isolation      string     `json:"isolation"`
	SupportReason  string     `json:"support_reason,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	Cols           int        `json:"cols"`
	Rows           int        `json:"rows"`
	Artifact       string     `json:"artifact"`
	Bytes          int64      `json:"bytes"`
	Truncated      bool       `json:"truncated"`
}

type RecordingRecord struct {
	TerminalRecording
	OrganizationID string
	ProjectID      string
	Cast           string
}

func (r RecordingRecord) Resource() TerminalRecording {
	recording := r.TerminalRecording
	recording.Artifact = RecordingPath(r)
	return recording
}

func RecordingPath(r RecordingRecord) string {
	if r.Bytes <= 0 {
		return ""
	}
	return "/api/v2/organizations/" + r.OrganizationID + "/projects/" + r.ProjectID +
		"/workspaces/" + r.WorkspaceID + "/terminal-recordings/" + r.ID
}

const RecordingPage = 50

func RecordingReadable(credential hubauth.Credential, record RecordingRecord) bool {
	role := credential.HostedRole
	if credential.Hosted == nil {
		return true
	}
	if credential.Hosted.Subject != "" && credential.Hosted.Subject == record.Subject {
		return true
	}
	if record.Isolation == workspacesession.IsolationUser {
		return role == "owner"
	}
	return role == "owner" || role == "admin"
}
