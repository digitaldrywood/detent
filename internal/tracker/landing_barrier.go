package tracker

import "github.com/digitaldrywood/detent/internal/gate"

type LandingBarrier struct {
	ProjectID     ProjectID              `json:"project_id"`
	CommandDigest string                 `json:"command_digest,omitempty"`
	ID            string                 `json:"id"`
	Repository    string                 `json:"repository"`
	BaseRef       string                 `json:"base_ref"`
	Pending       bool                   `json:"pending"`
	Running       bool                   `json:"running"`
	Red           bool                   `json:"red"`
	Repair        NativeWorkItemID       `json:"repair,omitempty"`
	Owner         string                 `json:"owner,omitempty"`
	Started       int64                  `json:"started"`
	Green         int64                  `json:"green"`
	Result        *gate.CommandResult    `json:"result,omitempty"`
	Changes       []NativeLandingReceipt `json:"changes,omitempty"`
}

type LandingBarrierRequest struct {
	Recover bool `json:"recover,omitempty"`
	Mutation
	Repository string              `json:"repository"`
	PolicyID   string              `json:"policy_id,omitempty"`
	ID         string              `json:"id,omitempty"`
	Action     string              `json:"action"`
	Result     *gate.CommandResult `json:"result,omitempty"`
}
