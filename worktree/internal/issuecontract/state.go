package issuecontract

import "time"

type ReleaseDeliveryApproval struct {
	PolicyID   string `json:"policy_id"`
	BodySHA256 string `json:"body_sha256"`
}

type State struct {
	ReleaseDelivery   *ReleaseDeliveryApproval `json:"release_delivery,omitempty"`
	Exempt            bool                     `json:"exempt,omitempty"`
	ConfirmedSections map[string]string        `json:"confirmed_sections,omitempty"`
	HumanAction       string                   `json:"human_action,omitempty"`
	ReturnState       string                   `json:"return_state,omitempty"`
	RecordedAt        time.Time                `json:"recorded_at,omitzero"`
}
