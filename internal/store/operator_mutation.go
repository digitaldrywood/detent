package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/mutation"
)

// OperatorReceipt uses the existing operator event record. Pending effects are
// never taken over: uncertain external outcomes must be inspected by an operator.
type OperatorReceipt struct {
	ConfigurationJSON json.RawMessage `json:"configuration_receipt,omitempty"`
	Revision          int64           `json:"revision,omitempty"`
	CommentID         string          `json:"comment_id,omitempty"`
	mutation.Metadata
	Outcome     string    `json:"outcome"`
	Identifier  string    `json:"identifier,omitempty"`
	URL         string    `json:"url,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

type OperatorMutations interface {
	OperatorMutation(context.Context, mutation.Metadata) (OperatorReceipt, bool, error)
	ReserveOperatorMutation(context.Context, mutation.Metadata) (bool, error)
	ClaimOperatorMutation(context.Context, mutation.Metadata) (bool, error)
	CompleteOperatorMutation(context.Context, OperatorReceipt) error
}
