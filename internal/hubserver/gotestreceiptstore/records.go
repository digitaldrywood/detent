package gotestreceiptstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Attestation struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	ID             string
	CredentialID   string
	RunnerID       string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type Receipt struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	BinaryKey      string
	InputsDigest   string
	Inputs         []string
	Package        string
	RunnerID       string
	RecordedAt     time.Time
	AttestationID  string
}

type Reader interface {
	GoTestReceiptAttestation(context.Context, tracker.ProjectID, string, string) (Attestation, error)
	GoTestReceipts(context.Context, tracker.ProjectID, string, time.Time, int) ([]gotestreceipt.Candidate, error)
}

type Writer interface {
	Reader
	CreateGoTestReceiptAttestation(context.Context, Attestation) error
	PutGoTestReceipt(context.Context, Receipt, int) error
	PruneGoTestReceipts(context.Context, time.Time, time.Time, int) (int64, int64, error)
}
