package platformstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

var importAmount = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?$`)

func (i *Importer) prepareAudit(ctx context.Context, row *importRow) error {
	v := row.values
	event := row.text["event"]
	if strings.HasPrefix(event, "support_requested:") || strings.HasPrefix(event, "support_started:") {
		kind, reason, _ := strings.Cut(event, ":")
		v["event"], v["reason"] = kind, reason
	} else if strings.HasPrefix(strings.TrimSpace(event), "{") {
		var kind struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(event), &kind) != nil || kind.Kind == "" {
			return ErrImport
		}
		v["event"] = kind.Kind
		switch kind.Kind {
		case "ai_credits_adjusted":
			var credit struct {
				Kind   string `json:"kind"`
				Retry  string `json:"idempotency_key"`
				Amount string `json:"amount_usd"`
				Reason string `json:"reason"`
			}
			if err := decodeImportJSON([]byte(event), &credit); err != nil {
				return err
			}
			if credit.Retry == "" || !importAmount.MatchString(credit.Amount) || len(credit.Amount) > 200 {
				return ErrImport
			}
			v["retry_identity"], v["amount_usd"], v["reason"] = credit.Retry, credit.Amount, credit.Reason
		case "operator_administration":
			var receipt struct {
				Kind    string               `json:"kind"`
				Retry   string               `json:"retry"`
				Hash    string               `json:"hash"`
				Outcome string               `json:"outcome"`
				Output  operatoradmin.Output `json:"output"`
			}
			if err := decodeImportJSON([]byte(event), &receipt); err != nil {
				return err
			}
			if receipt.Retry == "" || receipt.Hash == "" {
				return ErrImport
			}
			key, err := i.storeObject(ctx, receipt.Output.Data)
			if err != nil {
				return err
			}
			var signOut operatortool.SignOutResult
			if receipt.Output.SignOut != nil {
				signOut = *receipt.Output.SignOut
			}
			row.writes = append(row.writes, importWrite{table: "administration_receipts", values: map[string]any{
				"subject": v["subject"], "organization_id": v["organization_id"], "retry_identity": receipt.Retry,
				"input_hash": receipt.Hash, "outcome": receipt.Outcome, "resource_id": receipt.Output.ResourceID, "url": receipt.Output.URL,
				"reconnect": receipt.Output.Reconnect, "sign_out": receipt.Output.SignOut != nil, "signed_out": signOut.SignedOut,
				"provider_confirmed": signOut.ProviderConfirmed, "audit_recorded": signOut.AuditRecorded,
				"output_object_key": key, "recorded_at": v["recorded_at"], "import_source_id": v["id"],
			}})
		}
	} else {
		return nil
	}
	key, err := i.storeObject(ctx, []byte(event))
	if err != nil {
		return err
	}
	v["event_object_key"] = key
	return nil
}

func (i *Importer) storeObject(ctx context.Context, data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if len(data) > 1<<20 {
		return "", ErrImport
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	if compressed.Len() > 1<<20 {
		return "", ErrImport
	}
	digest := sha256.Sum256(compressed.Bytes())
	hash := hex.EncodeToString(digest[:])
	key := "platform/entry/sha256/" + hash
	if err := i.source.Objects.Put(ctx, key, bytes.NewReader(compressed.Bytes()), int64(compressed.Len()), "application/gzip", hash); err != nil {
		return "", err
	}
	reader, err := i.source.Objects.Open(ctx, key)
	if err != nil {
		return "", err
	}
	stored, readErr := io.ReadAll(io.LimitReader(reader, int64(compressed.Len())+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return "", err
	}
	if !bytes.Equal(stored, compressed.Bytes()) {
		return "", ErrImport
	}
	return key, nil
}
