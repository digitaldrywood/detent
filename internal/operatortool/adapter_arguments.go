package operatortool

import (
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AdministrationArguments struct {
	Grants           []AdministrationGrant `json:"grants,omitzero"`
	RequestID        string                `json:"request_id,omitempty"`
	Offset           int                   `json:"offset,omitempty"`
	Limit            int                   `json:"limit,omitempty"`
	OrganizationID   string                `json:"organization_id,omitempty"`
	Name             string                `json:"name,omitempty"`
	ConfirmName      string                `json:"confirm_name,omitempty"`
	InvitationID     string                `json:"invitation_id,omitempty"`
	MemberID         string                `json:"member_id,omitempty"`
	Email            string                `json:"email,omitempty"`
	Role             string                `json:"role,omitempty"`
	ProjectID        string                `json:"project_id,omitempty"`
	Write            bool                  `json:"write,omitempty"`
	Runner           bool                  `json:"runner,omitempty"`
	Revoke           bool                  `json:"revoke,omitempty"`
	CredentialID     string                `json:"credential_id,omitempty"`
	Scopes           []string              `json:"scopes,omitempty"`
	ProjectAccess    string                `json:"project_access,omitempty"`
	ExpectedRevision int64                 `json:"expected_revision,omitempty"`
	ProjectIDs       []string              `json:"project_ids,omitempty"`
	ExpiresIn        string                `json:"expires_in,omitempty"`
	Grace            string                `json:"grace,omitempty"`
	Reason           string                `json:"reason,omitempty"`
}

type AdministrationGrant struct {
	ProjectID string `json:"project_id"`
	Write     bool   `json:"write"`
	Runner    bool   `json:"runner"`
}

type HubFleetArguments struct {
	Release      bool                         `json:"release,omitempty"`
	FromRelease  bool                         `json:"from_release,omitempty"`
	Backend      string                       `json:"backend,omitempty"`
	ProjectID    string                       `json:"project_id,omitempty"`
	RequestID    string                       `json:"request_id,omitempty"`
	RunnerID     string                       `json:"runner_id,omitempty"`
	MachineID    string                       `json:"machine_id,omitempty"`
	EnrollmentID string                       `json:"enrollment_id,omitempty"`
	Enrollment   runnerauth.EnrollmentRequest `json:"enrollment,omitempty"`
	Change       json.RawMessage              `json:"change,omitempty"`
	Limit        int                          `json:"limit,omitempty"`
	Cursor       string                       `json:"cursor,omitempty"`
	Offset       int                          `json:"offset,omitempty"`
}

type FleetArguments struct {
	ProjectID   string          `json:"project_id,omitempty"`
	RequestID   string          `json:"request_id,omitempty"`
	Reference   string          `json:"reference,omitempty"`
	RunnerID    string          `json:"runner_id,omitempty"`
	MachineID   string          `json:"machine_id,omitempty"`
	Scope       string          `json:"scope,omitempty"`
	Recovery    string          `json:"recovery,omitempty"`
	Host        string          `json:"host,omitempty"`
	Since       string          `json:"since,omitempty"`
	Limit       int             `json:"limit,omitempty"`
	Offset      int             `json:"offset,omitempty"`
	Release     bool            `json:"release,omitempty"`
	FromRelease bool            `json:"from_release,omitempty"`
	WarningIDs  []string        `json:"warning_ids,omitempty"`
	AttemptID   int64           `json:"attempt_id,omitempty"`
	Action      string          `json:"action,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Change      json.RawMessage `json:"change,omitempty"`
}

type WorkspaceArguments struct {
	Path              string           `json:"path"`
	ShowIgnored       bool             `json:"show_ignored"`
	ProjectID         string           `json:"project_id"`
	RequestID         string           `json:"request_id"`
	WorkspaceID       string           `json:"workspace_id"`
	ConversationID    string           `json:"conversation_id"`
	AttachmentID      string           `json:"attachment_id"`
	ActionID          string           `json:"action_id"`
	RunID             string           `json:"run_id"`
	RecordingID       string           `json:"recording_id"`
	WorkItemID        string           `json:"work_item_id"`
	SubjectWorkItemID string           `json:"subject_work_item_id"`
	Cursor            string           `json:"cursor"`
	Query             string           `json:"query"`
	State             string           `json:"state"`
	Settled           *bool            `json:"settled"`
	Before            int64            `json:"before"`
	Limit             int              `json:"limit"`
	Offset            int              `json:"offset"`
	Length            int              `json:"length"`
	ExpectedRevision  tracker.Revision `json:"expected_revision"`
	Input             json.RawMessage  `json:"input"`
}

type HostedEventArguments struct {
	ProjectID   string `json:"project_id"`
	WorkspaceID string `json:"workspace_id"`
	Cursor      string `json:"cursor"`
}

type BudgetArguments struct {
	ProjectID      string   `json:"project_id"`
	PerDayMaxUSD   *float64 `json:"per_day_max_usd,omitempty"`
	PerIssueMaxUSD *float64 `json:"per_issue_max_usd,omitempty"`
	Duration       string   `json:"duration,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}

type MonthlyUsageArguments struct {
	ProjectID string `json:"project_id,omitempty"`
	Month     string `json:"month,omitempty"`
	Scope     string `json:"scope,omitempty"`
}

type UsageArguments struct {
	ProjectID string `json:"project_id"`
	By        string `json:"by"`
	From      string `json:"from"`
	To        string `json:"to"`
}

type OperatorChatReadArguments struct {
	Limit int `json:"limit"`
}

type OperatorChatArguments struct {
	ProjectID string `json:"project_id"`
	RequestID string `json:"request_id"`
	Message   string `json:"message"`
}

type StopRunArguments struct {
	ProjectID   string `json:"project_id"`
	Identifier  string `json:"identifier"`
	Destination string `json:"destination"`
	Priority    int    `json:"priority"`
	Reason      string `json:"reason"`
}

type CheckoutArguments struct {
	RequestID string `json:"request_id"`
	Price     string `json:"price"`
}

type PortalArguments struct {
	RequestID string `json:"request_id"`
}

type AutoFundArguments struct {
	RequestID string  `json:"request_id"`
	Price     *string `json:"price"`
	Enabled   *bool   `json:"enabled"`
	Threshold *int64  `json:"threshold_cents"`
}

type HostedUsageArguments struct {
	ProjectID string `json:"project_id"`
	Range     string `json:"range"`
}

type ActionResultArguments struct {
	ActionID string `json:"action_id"`
}

type ConversationAttachmentArguments struct {
	Name    string `json:"name"`
	MIME    string `json:"mime"`
	Content string `json:"content_base64"`
}
