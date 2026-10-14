package issuecontract

import "time"

type State struct {
	Exempt            bool              `json:"exempt,omitempty"`
	ConfirmedSections map[string]string `json:"confirmed_sections,omitempty"`
	HumanAction       string            `json:"human_action,omitempty"`
	ReturnState       string            `json:"return_state,omitempty"`
	RecordedAt        time.Time         `json:"recorded_at,omitzero"`
}
