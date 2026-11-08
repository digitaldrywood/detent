package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ConversationBillingImportSource struct {
	BundlePath string
	neutral    *VerifiedNeutralImport
	CopyPath   string
	Catalog    storagecatalog.Catalog
	Source     storagecatalog.Source
	Schema     storagecatalog.Schema
	Objects    attachment.DetailStore
	snapshotID string
}

type ConversationBillingImportResult struct {
	Prices int64
	Usage  int64
}

func (r *Repository) ImportConversationBilling(ctx context.Context, source ConversationBillingImportSource) (ConversationBillingImportResult, error) {
	if source.CopyPath != "" {
		return ConversationBillingImportResult{}, ErrNeutralImport

	}

	var result ConversationBillingImportResult
	if source.BundlePath == "" || source.Objects == nil || source.Source.Store != "hub" || strings.TrimSpace(source.Source.OrganizationID) != source.Source.OrganizationID || source.Source.OrganizationID == "" {
		return result, errors.New("invalid conversation billing import source")
	}
	if source.Schema == nil {
		source.Schema = schemaFromCatalog(source.Catalog)
	}
	source.Source.Schema = "hub_copy"
	if _, err := source.Catalog.Bind(source.Source, source.Schema); err != nil {
		return result, err
	}
	organization := tracker.OrganizationID(source.Source.OrganizationID)

	err := ReadNeutralImportBundle(ctx, source.BundlePath, organization, func(bundle *VerifiedNeutralImport) error {
		for _, table := range []struct {
			name string
			keys []string
		}{{"conversation_prices", []string{"id"}}, {"conversation_usage", []string{"organization_id", "turn_id"}}} {
			if err := bundle.ValidateCatalogTable(source.Catalog, source.Schema, table.name, table.keys); err != nil {
				return err
			}
		}
		if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		source.neutral, source.snapshotID = bundle, bundle.Identity().SourceSHA256
		var err error
		result, err = r.importConversationBillingCopy(ctx, source)
		return err
	})
	return result, err
}

func (r *Repository) importConversationBillingCopy(ctx context.Context, source ConversationBillingImportSource) (ConversationBillingImportResult, error) {
	var result ConversationBillingImportResult
	encoded, err := json.Marshal(struct {
		Catalog storagecatalog.Catalog
		Schema  storagecatalog.Schema
	}{source.Catalog, source.Schema})
	if err != nil {
		return result, err
	}
	var frozen struct {
		Catalog storagecatalog.Catalog
		Schema  storagecatalog.Schema
	}
	if err := json.Unmarshal(encoded, &frozen); err != nil {
		return result, err
	}
	source.Catalog, source.Schema = frozen.Catalog, frozen.Schema
	digest := sha256.Sum256(encoded)
	catalogDigest := hex.EncodeToString(digest[:])
	binding, err := source.Catalog.Bind(source.Source, source.Schema)
	if err != nil {
		return result, err
	}
	organization := tracker.OrganizationID(source.Source.OrganizationID)
	if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		if _, err := s.db.Exec(ctx, `INSERT INTO conversation_billing_imports(organization_id,source_schema,snapshot_id,catalog_digest) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id) DO NOTHING`, organization, source.Source.Schema, source.snapshotID, catalogDigest); err != nil {
			return err
		}
		var sourceSchema, snapshot, catalog string
		if err := s.db.QueryRow(ctx, `SELECT source_schema,snapshot_id,catalog_digest FROM conversation_billing_imports WHERE organization_id=$1`, organization).Scan(&sourceSchema, &snapshot, &catalog); err != nil {
			return err
		}
		if sourceSchema != source.Source.Schema || snapshot != source.snapshotID || catalog != catalogDigest {
			return mutation.ErrConflict
		}
		return nil
	}); err != nil {
		return result, err
	}
	for _, table := range []struct {
		name   string
		keys   []string
		count  *int64
		codecs map[string]string
	}{
		{"conversation_prices", []string{"id"}, &result.Prices, map[string]string{"id": "int64", "provider": "text", "model": "text", "effective_at": "int64", "input": "float64", "cached_input": "float64", "output": "float64"}},
		{"conversation_usage", []string{"organization_id", "turn_id"}, &result.Usage, map[string]string{"organization_id": "text", "project_id": "text", "conversation_id": "text", "turn_id": "text", "provider": "text", "model": "text", "occurred_at": "int64", "input": "int64", "cached_input": "int64", "output": "int64", "reasoning_output": "int64", "outcome": "text", "price_id": "int64", "cost_usd": "float64", "cache_savings_usd": "float64"}},
	} {
		columns := make([]string, 0, len(source.Schema[table.name]))
		definition := source.Catalog.Tables["hub/"+table.name]
		for name, signature := range source.Schema[table.name] {
			mapping, ok := definition.Columns[name]
			if !ok || mapping.Source != signature || mapping.Disposition != "column" || mapping.Codec != table.codecs[name] {
				return result, storagecatalog.ErrUnmapped
			}
			columns = append(columns, name)
		}
		slices.Sort(columns)
		if len(columns) != len(table.codecs) || len(columns) != len(definition.Columns) {
			return result, storagecatalog.ErrUnmapped
		}
		var after []any
		for {
			page, next, err := readConversationBillingPage(ctx, source, binding, table.name, columns, table.keys, after)
			if err != nil {
				return result, err
			}
			after = next
			if len(page) == 0 {
				break
			}
			if err := r.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
				for _, values := range page {
					if err := importConversationBillingRow(ctx, w, organization, table.name, values); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return result, err
			}
			*table.count += int64(len(page))
		}
	}
	err = r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		written, err := s.db.Exec(ctx, `UPDATE conversation_billing_imports SET completed=true,price_rows=$5,usage_rows=$6 WHERE organization_id=$1 AND source_schema=$2 AND snapshot_id=$3 AND catalog_digest=$4 AND (NOT completed OR (price_rows=$5 AND usage_rows=$6))`, organization, source.Source.Schema, source.snapshotID, catalogDigest, result.Prices, result.Usage)
		if err != nil {
			return err
		}
		count := written.RowsAffected()
		if count != 1 {
			return mutation.ErrConflict
		}
		return nil
	})
	return result, err
}

func importConversationBillingRow(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID, table string, v map[string]storagecatalog.Value) error {
	if table == "conversation_prices" {
		return w.CreateConversationPrice(ctx, billingstore.ConversationPrice{OrganizationID: organization, ID: v["id"].Integer, Provider: v["provider"].Text, Model: v["model"].Text, EffectiveAt: time.UnixMicro(v["effective_at"].Integer).UTC(), Input: v["input"].Real, CachedInput: v["cached_input"].Real, Output: v["output"].Real})
	}
	usage := billingstore.ConversationUsage{OrganizationID: tracker.OrganizationID(v["organization_id"].Text), ProjectID: tracker.ProjectID(v["project_id"].Text), ConversationID: v["conversation_id"].Text, TurnID: v["turn_id"].Text, Provider: v["provider"].Text, Model: v["model"].Text, OccurredAt: time.UnixMicro(v["occurred_at"].Integer).UTC(), Input: v["input"].Integer, CachedInput: v["cached_input"].Integer, Output: v["output"].Integer, ReasoningOutput: v["reasoning_output"].Integer, Outcome: v["outcome"].Text}
	if !v["price_id"].Null {
		usage.PriceID = new(v["price_id"].Integer)
	}
	if !v["cost_usd"].Null {
		usage.CostUSD = new(v["cost_usd"].Real)
	}
	if !v["cache_savings_usd"].Null {
		usage.CacheSavingsUSD = new(v["cache_savings_usd"].Real)
	}
	_, err := w.RecordConversationUsage(ctx, usage)
	return err
}

func readConversationBillingPage(ctx context.Context, source ConversationBillingImportSource, binding *storagecatalog.Binding, table string, columns, keys []string, after []any) ([]map[string]storagecatalog.Value, []any, error) {
	return readNeutralConversationBillingPage(ctx, source, binding, table, columns, keys, after)
}
