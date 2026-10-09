package tracker

import (
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
)

type LandingBarrier struct {
	ProjectID     ProjectID              `json:"project_id"`
	CommandDigest string                 `json:"command_digest,omitempty"`
	ID            string                 `json:"id"`
	Repository    string                 `json:"repository"`
	BaseRef       string                 `json:"base_ref"`
	Running       bool                   `json:"running"`
	Red           bool                   `json:"red"`
	Owner         string                 `json:"owner,omitempty"`
	Started       int64                  `json:"started"`
	Green         int64                  `json:"green"`
	GreenHead     string                 `json:"green_head,omitempty"`
	Result        *gate.CommandResult    `json:"result,omitempty"`
	History       []LandingBarrierEvent  `json:"history,omitempty"`
	HistoryCursor int64                  `json:"history_cursor,omitempty"`
	Changes       []NativeLandingReceipt `json:"changes,omitempty"`
}

type LandingBarrierRequest struct {
	Recover bool `json:"recover,omitempty"`
	Mutation
	Repository string                `json:"repository"`
	PolicyID   string                `json:"policy_id,omitempty"`
	Head       string                `json:"head,omitempty"`
	ID         string                `json:"id,omitempty"`
	Repair     *LandingBarrierRepair `json:"repair,omitempty"`
	Action     string                `json:"action"`
	Result     *gate.CommandResult   `json:"result,omitempty"`
}

type LandingBarrierEvent struct {
	ID     string                `json:"id"`
	Owner  string                `json:"owner"`
	At     time.Time             `json:"at"`
	Result *gate.CommandResult   `json:"result,omitempty"`
	Repair *LandingBarrierRepair `json:"repair,omitempty"`
}
type LandingBarrierRepair struct {
	HeadSHA  string `json:"head_sha"`
	ThreadID string `json:"thread_id,omitempty"`
	TurnID   string `json:"turn_id,omitempty"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}
