package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ConversationBillingImportSource struct {
	DB         *sql.DB
	Catalog    storagecatalog.Catalog
	Source     storagecatalog.Source
	Schema     storagecatalog.Schema
	SnapshotID string
}

type ConversationBillingImportResult struct {
	Prices int64
	Usage  int64
}

func (r *Repository) ImportConversationBilling(ctx context.Context, source ConversationBillingImportSource) (ConversationBillingImportResult, error) {
	var result ConversationBillingImportResult
	if source.DB == nil || source.Source.Store != "hub" || strings.TrimSpace(source.SnapshotID) == "" || len(source.SnapshotID) > 200 {
		return result, errors.New("invalid conversation billing import source")
	}
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
	tx, err := source.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET TRANSACTION SNAPSHOT "+pq.QuoteLiteral(source.SnapshotID)); err != nil {
		return result, err
	}
	organization := tracker.OrganizationID(source.Source.OrganizationID)
	if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO conversation_billing_imports(organization_id,source_schema,snapshot_id,catalog_digest) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id) DO NOTHING`, organization, source.Source.Schema, source.SnapshotID, catalogDigest); err != nil {
			return err
		}
		var sourceSchema, snapshot, catalog string
		if err := s.db.QueryRowContext(ctx, `SELECT source_schema,snapshot_id,catalog_digest FROM conversation_billing_imports WHERE organization_id=$1`, organization).Scan(&sourceSchema, &snapshot, &catalog); err != nil {
			return err
		}
		if sourceSchema != source.Source.Schema || snapshot != source.SnapshotID || catalog != catalogDigest {
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
		qualified := pq.QuoteIdentifier(source.Source.Schema) + "." + pq.QuoteIdentifier(table.name)
		if err := validateConversationBillingSourceColumns(ctx, tx, qualified, columns); err != nil {
			return result, err
		}
		var valid bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am am ON am.oid=c.relam
WHERE i.indrelid=$1::regclass AND i.indisvalid AND i.indisprimary AND i.indpred IS NULL AND i.indexprs IS NULL
AND am.amname='btree' AND i.indnkeyatts=cardinality($2::text[])
AND NOT EXISTS (SELECT 1 FROM unnest(i.indoption) option WHERE option<>0)
AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(attnum,n)
JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum WHERE k.n<=i.indnkeyatts ORDER BY k.n)=$2::text[]
AND NOT EXISTS (SELECT 1 FROM unnest(i.indcollation) WITH ORDINALITY k(coll,n)
WHERE k.n<=i.indnkeyatts AND k.coll<>'"C"'::regcollation))`, qualified, pq.Array(table.keys)).Scan(&valid)
		if err != nil {
			return result, err
		}
		if !valid {
			return result, errors.New("conversation billing source requires ordered primary key")
		}
		var after []any
		for {
			page, next, err := readConversationBillingPage(ctx, tx, source, binding, qualified, table.name, columns, table.keys, after)
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
	if err := tx.Commit(); err != nil {
		return result, err
	}
	err = r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		written, err := s.db.ExecContext(ctx, `UPDATE conversation_billing_imports SET completed=true,price_rows=$5,usage_rows=$6 WHERE organization_id=$1 AND source_schema=$2 AND snapshot_id=$3 AND catalog_digest=$4 AND (NOT completed OR (price_rows=$5 AND usage_rows=$6))`, organization, source.Source.Schema, source.SnapshotID, catalogDigest, result.Prices, result.Usage)
		if err != nil {
			return err
		}
		count, err := written.RowsAffected()
		if err != nil {
			return err
		}
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

func validateConversationBillingSourceColumns(ctx context.Context, tx *sql.Tx, qualified string, columns []string) error {
	rows, err := tx.QueryContext(ctx, `SELECT attname,atttypid='text'::regtype FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped`, qualified)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var name string
		var textType bool
		if err := rows.Scan(&name, &textType); err != nil {
			return err
		}
		if !slices.Contains(columns, name) || !textType {
			return storagecatalog.ErrUnmapped
		}
		seen++
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	if seen != len(columns) {
		return storagecatalog.ErrUnmapped
	}

	return nil
}

func readConversationBillingPage(ctx context.Context, tx *sql.Tx, source ConversationBillingImportSource, binding *storagecatalog.Binding, qualified, table string, columns, keys []string, after []any) ([]map[string]storagecatalog.Value, []any, error) {
	quoted := make([]string, len(columns))
	for n, name := range columns {
		quoted[n] = pq.QuoteIdentifier(name)
	}
	order := make([]string, len(keys))
	for n, name := range keys {
		order[n] = pq.QuoteIdentifier(name) + ` COLLATE "C"`
	}
	query := strings.Join([]string{"SELECT", strings.Join(quoted, ","), "FROM", qualified}, " ")
	if len(after) > 0 {
		placeholders := make([]string, len(after))
		for n := range after {
			placeholders[n] = fmt.Sprintf("$%d::text COLLATE \"C\"", n+1)
		}
		query += " WHERE (" + strings.Join(order, ",") + ") > (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY " + strings.Join(order, ",") + " LIMIT 100"
	rows, err := tx.QueryContext(ctx, query, after...)
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
			after[n] = string(raw[key].Data)
		}
		page = append(page, values)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}

	return page, after, nil
}
