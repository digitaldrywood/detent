package tracker

import (
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
)

type LandingBarrier struct {
	ReadyAt        time.Time              `json:"ready_at,omitzero"`
	ClaimStartedAt time.Time              `json:"claim_started_at,omitzero"`
	ClaimedAt      time.Time              `json:"claimed_at,omitzero"`
	ProjectID      ProjectID              `json:"project_id"`
	CommandDigest  string                 `json:"command_digest,omitempty"`
	ID             string                 `json:"id"`
	Repository     string                 `json:"repository"`
	BaseRef        string                 `json:"base_ref"`
	Running        bool                   `json:"running"`
	Red            bool                   `json:"red"`
	Repair         NativeWorkItemID       `json:"repair,omitempty"`
	Owner          string                 `json:"owner,omitempty"`
	Started        int64                  `json:"started"`
	Green          int64                  `json:"green"`
	GreenHead      string                 `json:"green_head,omitempty"`
	Result         *gate.CommandResult    `json:"result,omitempty"`
	Changes        []NativeLandingReceipt `json:"changes,omitempty"`
}

type LandingBarrierRequest struct {
	ClaimStartedAt time.Time `json:"claim_started_at,omitzero"`
	Recover        bool      `json:"recover,omitempty"`
	Mutation
	Repository string              `json:"repository"`
	PolicyID   string              `json:"policy_id,omitempty"`
	Head       string              `json:"head,omitempty"`
	ID         string              `json:"id,omitempty"`
	Action     string              `json:"action"`
	Result     *gate.CommandResult `json:"result,omitempty"`
}
