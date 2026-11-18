package receiptstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const PendingPayload = `{"detent_pending_command":true}`
const ReplayRetention = 24 * time.Hour
const MaxRetentionBatch = 5000
const MaxImportBatch = 500

var ErrExpired = errors.New("the command completed; its stored response is no longer available; the command will not be repeated")
var ErrImmutableReceiptPresent = errors.New("metadata import intersects an immutable response")

type Record struct {
	OrganizationID             tracker.OrganizationID
	ActorID                    string
	Operation                  string
	CommandKey                 string
	RequestHash                string
	CreatedAt                  time.Time
	PayloadObjectKey           string
	PayloadSHA256              string
	PayloadBytes               int64
	PayloadContentBytes        int64
	ResponseClaimed            bool
	ResponsePending            bool
	ResponseRequestID          string
	ResponseExpired            bool
	ResponseReplayUntilNs      int64
	ResponseReconciliationHash string
}

type Writer interface {
	ClaimReceipt(context.Context, Record) (Record, bool, error)
	CompleteReceipt(context.Context, Record) (Record, error)
	AbandonReceipt(context.Context, string, string, string, string) error
}

type ActorReader interface {
	ReceiptMember(context.Context, string) (accessstore.Member, error)
}

type RetentionReader interface {
	ReceiptExpiryPage(context.Context, time.Time, int) ([]Record, error)
}

type RetentionWriter interface {
	ExpireReceipt(context.Context, Record, time.Time) (bool, error)
}

type Import struct {
	Record     Record
	SnapshotID string
	SourceHash string
	ObservedAt time.Time
}

type ImportReader interface {
	VerifyReceiptStates(context.Context, []Import) error
	ReceiptMetadataBodyCount(context.Context, []Record) (int64, error)
	ReceiptSource(context.Context, string, string, string) (string, string, error)
}

type ImportWriter interface {
	ImportReceiptStates(context.Context, []Import) error
	ImportReceipt(context.Context, Import) (Record, error)
}

func ResponseFacts(raw []byte) (bool, string, error) {
	if string(raw) == PendingPayload {
		return true, "", nil
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		if !json.Valid(raw) {
			return false, "", err
		}
		return false, "", nil
	}
	var pending bool
	if value := response["pending"]; len(value) > 0 {
		if err := json.Unmarshal(value, &pending); err != nil {
			var number float64
			pending = json.Unmarshal(value, &number) != nil || number != 0
		}
	}
	var request struct {
		ID string `json:"id"`
	}
	if value := response["request"]; len(value) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) == nil {
			if value := fields["id"]; len(value) > 0 {
				if err := json.Unmarshal(value, &request.ID); err != nil {
					request.ID = ""
				}
			}
		}
	}
	return pending, request.ID, nil
}
