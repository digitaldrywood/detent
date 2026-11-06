package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	CopyPath   string
	Catalog    storagecatalog.Catalog
	Source     storagecatalog.Source
	Schema     storagecatalog.Schema
	Objects    attachment.DetailStore
	copyTx     *sql.Tx
	snapshotID string
}

type ConversationBillingImportResult struct {
	Prices int64
	Usage  int64
}

func (r *Repository) ImportConversationBilling(ctx context.Context, source ConversationBillingImportSource) (ConversationBillingImportResult, error) {
	var result ConversationBillingImportResult
	if source.CopyPath == "" || source.Objects == nil || source.Source.Store != "hub" || strings.TrimSpace(source.Source.OrganizationID) != source.Source.OrganizationID || source.Source.OrganizationID == "" {
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
	err := ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, digest string) error {
		for _, table := range []string{"conversation_prices", "conversation_usage"} {
			keys := []string{"id"}
			if table == "conversation_usage" {
				keys = []string{"organization_id", "turn_id"}
			}
			if err := validateConversationBillingCopyTable(ctx, tx, source.Catalog, source.Schema, table, keys); err != nil {
				return err
			}
		}
		if err := r.PinCoreImportSource(ctx, organization, digest, source.Objects); err != nil {
			return err
		}
		source.copyTx, source.snapshotID = tx, digest
		imported, err := r.importConversationBillingCopy(ctx, source)
		result = imported
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
			page, next, err := readConversationBillingPage(ctx, source.copyTx, source, binding, table.name, columns, table.keys, after)
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

func validateConversationBillingCopyTable(ctx context.Context, tx *sql.Tx, catalog storagecatalog.Catalog, schema storagecatalog.Schema, table string, keys []string) error {
	if tx == nil || len(keys) == 0 {
		return storagecatalog.ErrUnmapped
	}
	definition, ok := catalog.Tables["hub/"+table]
	if !ok || len(definition.Columns) == 0 || len(schema[table]) != len(definition.Columns) {
		return storagecatalog.ErrUnmapped
	}
	rows, err := tx.QueryContext(ctx, `SELECT cid,name,type,"notnull",dflt_value,pk,hidden FROM pragma_table_xinfo(?) ORDER BY cid`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := make(map[string]bool, len(schema[table]))
	primary := make([]string, len(keys))
	for rows.Next() {
		var ordinal, notNull, primaryKey, hidden int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&ordinal, &name, &columnType, &notNull, &defaultValue, &primaryKey, &hidden); err != nil {
			return err
		}
		mapping, exists := definition.Columns[name]
		if !exists || schema[table][name] != mapping.Source {
			return storagecatalog.ErrUnmapped
		}
		var actualDefault any
		if defaultValue.Valid {
			actualDefault = defaultValue.String
		}
		actual, err := json.Marshal([]any{ordinal, name, columnType, notNull, actualDefault, primaryKey, hidden})
		if err != nil || !sameSourceSignature(actual, mapping.Source) {
			return storagecatalog.ErrUnmapped
		}
		if primaryKey > 0 {
			if primaryKey > len(primary) {
				return storagecatalog.ErrUnmapped
			}
			primary[primaryKey-1] = name
		}
		seen[name] = true
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	if len(seen) != len(schema[table]) || !slices.Equal(primary, keys) {
		return storagecatalog.ErrUnmapped
	}
	return validateImportCopyPrimaryIndex(ctx, tx, table, keys)
}

func readConversationBillingPage(ctx context.Context, tx *sql.Tx, source ConversationBillingImportSource, binding *storagecatalog.Binding, table string, columns, keys []string, after []any) ([]map[string]storagecatalog.Value, []any, error) {
	quoted := make([]string, len(columns))
	for n, name := range columns {
		quoted[n] = `CAST("` + strings.ReplaceAll(name, `"`, `""`) + `" AS TEXT)`
	}
	order := make([]string, len(keys))
	for n, name := range keys {
		order[n] = `"` + strings.ReplaceAll(name, `"`, `""`) + `" COLLATE BINARY`
	}
	query := strings.Join([]string{"SELECT", strings.Join(quoted, ","), "FROM", `"` + strings.ReplaceAll(table, `"`, `""`) + `"`}, " ")
	args := make([]any, 0, len(after)+1)
	if table == "conversation_usage" {
		query += " WHERE organization_id = ?"
		args = append(args, source.Source.OrganizationID)
	}
	if len(after) > 0 {
		placeholders := make([]string, len(after))
		for n := range after {
			placeholders[n] = "?"
		}
		clause := " WHERE "
		if table == "conversation_usage" {
			clause = " AND "
		}
		query += clause + "(" + strings.Join(order, ",") + ") > (" + strings.Join(placeholders, ",") + ")"
		args = append(args, after...)
	}
	query += " ORDER BY " + strings.Join(order, ",") + " LIMIT 100"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var page []map[string]storagecatalog.Value
	for rows.Next() {
		data := make([][]byte, len(columns))
		destinations := make([]any, len(columns))
		for n := range data {
			destinations[n] = &data[n]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, nil, err
		}
		raw := make(map[string]storagecatalog.Cell, len(columns))
		values := make(map[string]storagecatalog.Value, len(columns))
		for n, name := range columns {
			cell := storagecatalog.Cell{Encoding: "utf8", Data: data[n]}
			if data[n] == nil {
				cell = storagecatalog.Cell{Null: true}
			}
			raw[name] = cell
			value, err := source.Catalog.Decode("hub/"+table, name, cell)
			if err != nil {
				return nil, nil, err
			}
			values[name] = value
		}
		if _, err := binding.ValidateMappedRow(table, raw); err != nil {
			return nil, nil, err
		}
		after = make([]any, len(keys))
		for n, key := range keys {
			if raw[key].Null {
				return nil, nil, storagecatalog.ErrValue
			}
			value := values[key]
			switch source.Catalog.Tables["hub/"+table].Columns[key].Codec {
			case "int64":
				after[n] = value.Integer
			case "float64":
				after[n] = value.Real
			default:
				after[n] = value.Text
			}
		}
		page = append(page, values)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}

	return page, after, nil
}
