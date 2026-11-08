package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSlackContractPersistsSettingsAndRetryReceipts(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	now := historyAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		evidence, err := newHistoryObjects().StoreDetail(t.Context(), string(org), []byte("example evidence"))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"first", "second"} {
			if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
				_, err := w.CreateHealthRecord(t.Context(), postgres.CreateHealthRecordInput{OrganizationID: org, ID: id, Fingerprint: id, Signal: "signal", Class: "class", SubjectKind: "work_item", SubjectID: "wi_first_1", OpenedAt: now, LastSeenAt: now, Severity: "attention", Summary: "example", NextAction: "inspect", EvidenceObjectKey: evidence.Key, EvidenceSha256: evidence.SHA256, EvidenceBytes: evidence.CompressedSize, EvidenceContentBytes: evidence.UncompressedSize})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}

		if err := repository.WriteSlack(t.Context(), org, corestore.Serving, func(w postgres.SlackWriter) error {
			row, err := w.CreateSlackIntegration(t.Context(), postgres.CreateSlackIntegrationInput{OrganizationID: org, ChannelName: string(org), Enabled: true, UpdatedAt: now})
			if err != nil {
				return err
			}
			if row.Revision != 1 {
				t.Fatalf("settings=%+v", row)
			}
			for _, id := range []string{"first", "second"} {
				input := postgres.EnqueueSlackDeliveryInput{OrganizationID: org, ID: id, FindingID: id, Event: "opened", EventAt: now, QueuedAt: now}
				if _, err := w.EnqueueSlackDelivery(t.Context(), input); err != nil {
					return err
				}
				replay, err := w.EnqueueSlackDelivery(t.Context(), input)
				if err != nil {
					return err
				}
				if replay.Attempts != 0 {
					t.Fatalf("replay=%+v", replay)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteSlack(t.Context(), org, corestore.Background, func(w postgres.SlackWriter) error {
			row, err := w.ClaimSlackDelivery(t.Context(), postgres.ClaimSlackDeliveryInput{OrganizationID: org, ID: "first", ExpectedAttempts: 0, Now: now, NextAttemptAt: now.Add(time.Minute)})
			if err != nil {
				return err
			}
			if row.Attempts != 1 {
				t.Fatalf("claim=%+v", row)
			}
			_, err = w.FinishSlackDelivery(t.Context(), postgres.FinishSlackDeliveryInput{OrganizationID: org, ID: "first", ExpectedAttempts: 1, Outcome: "rejected", StatusCode: 503, At: now, NextAttemptAt: now.Add(time.Minute)})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		err = repository.WriteSlack(t.Context(), org, corestore.Background, func(w postgres.SlackWriter) error {
			_, err := w.ClaimSlackDelivery(t.Context(), postgres.ClaimSlackDeliveryInput{OrganizationID: org, ID: "first", ExpectedAttempts: 1, Now: now, NextAttemptAt: now.Add(time.Minute)})
			return err
		})
		if !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("early retry=%v", err)
		}
		if err := repository.WriteSlack(t.Context(), org, corestore.Background, func(w postgres.SlackWriter) error {
			if _, err := w.ClaimSlackDelivery(t.Context(), postgres.ClaimSlackDeliveryInput{OrganizationID: org, ID: "first", ExpectedAttempts: 1, Now: now.Add(time.Minute), NextAttemptAt: now.Add(2 * time.Minute)}); err != nil {
				return err
			}
			if _, err := w.FinishSlackDelivery(t.Context(), postgres.FinishSlackDeliveryInput{OrganizationID: org, ID: "first", ExpectedAttempts: 2, Outcome: "committed", ExternalReceipt: "receipt_" + string(org), StatusCode: 200, At: now.Add(time.Minute), NextAttemptAt: now.Add(2 * time.Minute)}); err != nil {
				return err
			}
			return w.RecordSlackStatus(t.Context(), postgres.RecordSlackStatusInput{OrganizationID: org, ExpectedRevision: 1, StatusCode: 200, At: now.Add(time.Minute)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := repository.WriteSlack(t.Context(), "org_first", corestore.Serving, func(w postgres.SlackWriter) error {
		_, err := w.UpdateSlackIntegration(t.Context(), postgres.UpdateSlackIntegrationInput{OrganizationID: "org_second", Revision: 1, UpdatedAt: now})
		return err
	})
	if !errors.Is(err, corestore.ErrTenantMismatch) {
		t.Fatalf("foreign settings=%v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadSlack(t.Context(), org, corestore.Serving, func(r postgres.SlackReader) error {
			settings, err := r.SlackIntegration(t.Context())
			if err != nil {
				return err
			}
			if settings.OrganizationID != org || settings.ChannelName != string(org) || settings.LastStatusCode != 200 || settings.LastSuccessAt == nil {
				t.Fatalf("settings=%+v", settings)
			}
			pending, err := r.PendingSlackDeliveries(t.Context(), postgres.PendingSlackDeliveriesInput{OrganizationID: org, NextAttemptAt: now.Add(2 * time.Minute), Limit: 2})
			if err != nil {
				return err
			}
			if len(pending) != 1 || pending[0].ID != "second" {
				t.Fatalf("pending=%+v", pending)
			}
			receipt, err := r.SlackDelivery(t.Context(), postgres.SlackDeliveryInput{OrganizationID: org, ID: "first"})
			if err != nil {
				return err
			}
			if receipt.Outcome != "committed" || receipt.Attempts != 2 || receipt.ExternalReceipt != "receipt_"+string(org) || receipt.CompletedAt == nil {
				t.Fatalf("receipt=%+v", receipt)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
