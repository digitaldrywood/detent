package postgres

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/gotestreceiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) GoTestReceiptAttestation(ctx context.Context, project tracker.ProjectID, id, credentialID string) (gotestreceiptstore.Attestation, error) {
	if project == "" || id == "" || credentialID == "" {
		return gotestreceiptstore.Attestation{}, corestore.ErrNotFound
	}
	row, err := s.queries.GoTestReceiptAttestation(ctx, pgquery.GoTestReceiptAttestationParams{OrganizationID: s.organization, ProjectID: project, ID: id, CredentialID: credentialID})
	if err != nil {
		return gotestreceiptstore.Attestation{}, mapError(ctx, err)
	}
	return goTestReceiptAttestationRecord(row), nil
}

func (s *session) GoTestReceipts(ctx context.Context, project tracker.ProjectID, key string, recordedAfter time.Time, limit int) ([]gotestreceipt.Candidate, error) {
	if project == "" || !gotestreceipt.ValidKey(key) || recordedAfter.IsZero() || limit < 1 || limit > gotestreceipt.MaxCandidates {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.GoTestReceipts(ctx, pgquery.GoTestReceiptsParams{OrganizationID: s.organization, ProjectID: project, BinaryKey: key, RecordedAfter: recordTime(recordedAfter), PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]gotestreceipt.Candidate, 0, len(rows))
	for _, row := range rows {
		inputs := []string{}
		if row.Inputs != "" {
			inputs = strings.Split(row.Inputs, "\n")
		}
		result = append(result, gotestreceipt.Candidate{Inputs: inputs, Digest: row.InputsDigest})
	}
	return result, nil
}

func (s *session) CreateGoTestReceiptAttestation(ctx context.Context, record gotestreceiptstore.Attestation) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	if record.ProjectID == "" || record.ID == "" || len(record.ID) > 128 || record.CredentialID == "" || record.RunnerID == "" || record.CreatedAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) {
		return eventstore.ErrInvalidRecord
	}
	err := s.queries.CreateGoTestReceiptAttestation(ctx, pgquery.CreateGoTestReceiptAttestationParams{OrganizationID: s.organization, ProjectID: record.ProjectID, ID: record.ID, CredentialID: record.CredentialID, RunnerID: record.RunnerID, CreatedAt: recordTime(record.CreatedAt), ExpiresAt: recordTime(record.ExpiresAt)})
	return mapError(ctx, err)
}

func (s *session) PutGoTestReceipt(ctx context.Context, record gotestreceiptstore.Receipt, keep int) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	input := gotestreceipt.Receipt{Attestation: record.AttestationID, Key: record.BinaryKey, Package: record.Package, Inputs: record.Inputs, Digest: record.InputsDigest}
	if record.ProjectID == "" || record.RunnerID == "" || record.RecordedAt.IsZero() || input.Validate() != nil || keep < 1 || keep > gotestreceipt.MaxCandidates {
		return eventstore.ErrInvalidRecord
	}
	lockKey := goTestReceiptLockKey(record.OrganizationID, record.ProjectID, record.BinaryKey)
	if err := s.queries.LockGoTestReceiptKey(ctx, lockKey); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.PutGoTestReceipt(ctx, pgquery.PutGoTestReceiptParams{OrganizationID: s.organization, ProjectID: record.ProjectID, BinaryKey: record.BinaryKey, InputsDigest: record.InputsDigest, Inputs: strings.Join(record.Inputs, "\n"), Package: record.Package, RunnerID: record.RunnerID, RecordedAt: recordTime(record.RecordedAt), AttestationID: record.AttestationID}); err != nil {
		return mapError(ctx, err)
	}
	return mapError(ctx, s.queries.EvictGoTestReceipts(ctx, pgquery.EvictGoTestReceiptsParams{OrganizationID: s.organization, ProjectID: record.ProjectID, BinaryKey: record.BinaryKey, KeepCount: int32(keep)}))
}

func goTestReceiptLockKey(organization tracker.OrganizationID, project tracker.ProjectID, binaryKey string) string {
	var key strings.Builder
	for _, part := range []string{string(organization), string(project), binaryKey} {
		key.WriteString(strconv.Itoa(len(part)))
		key.WriteByte(':')
		key.WriteString(part)
	}
	return key.String()
}

func (s *session) PruneGoTestReceipts(ctx context.Context, receiptCutoff, attestationCutoff time.Time, limit int) (int64, int64, error) {
	if receiptCutoff.IsZero() || attestationCutoff.IsZero() || limit < 1 || limit > 64 {
		return 0, 0, corestore.ErrInvalidPage
	}
	receipts, err := s.queries.PruneGoTestReceipts(ctx, pgquery.PruneGoTestReceiptsParams{OrganizationID: s.organization, ReceiptCutoff: recordTime(receiptCutoff), BatchLimit: int32(limit)})
	if err != nil {
		return 0, 0, mapError(ctx, err)
	}
	attestations, err := s.queries.PruneGoTestReceiptAttestations(ctx, pgquery.PruneGoTestReceiptAttestationsParams{OrganizationID: s.organization, AttestationCutoff: recordTime(attestationCutoff), BatchLimit: int32(limit)})
	if err != nil {
		return receipts, 0, mapError(ctx, err)
	}
	return receipts, attestations, nil
}

func goTestReceiptAttestationRecord(row pgquery.GoTestReceiptAttestation) gotestreceiptstore.Attestation {
	return gotestreceiptstore.Attestation{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ID: row.ID, CredentialID: row.CredentialID, RunnerID: row.RunnerID, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt}
}
