package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CostImportSource struct {
	DB         *sql.DB
	Catalog    storagecatalog.Catalog
	Source     storagecatalog.Source
	Schema     storagecatalog.Schema
	SnapshotID string
	Objects    attachment.DetailStore
}

type CostImporter struct {
	repository   *Repository
	source       CostImportSource
	binding      *storagecatalog.Binding
	digest       string
	organization tracker.OrganizationID
}

func (r *Repository) NewCostImporter(ctx context.Context, source CostImportSource) (*CostImporter, error) {
	if source.DB == nil || source.Objects == nil || source.Source.Store != "hub" || source.SnapshotID == "" || len(source.SnapshotID) > 200 || strings.TrimSpace(source.SnapshotID) != source.SnapshotID {
		return nil, ErrCostImport
	}
	encoded, err := json.Marshal(struct {
		Catalog storagecatalog.Catalog
		Schema  storagecatalog.Schema
	}{source.Catalog, source.Schema})
	if err != nil {
		return nil, ErrCostImport
	}
	var frozen struct {
		Catalog storagecatalog.Catalog
		Schema  storagecatalog.Schema
	}
	if err := json.Unmarshal(encoded, &frozen); err != nil {
		return nil, ErrCostImport
	}
	source.Catalog, source.Schema = frozen.Catalog, frozen.Schema
	binding, err := source.Catalog.Bind(source.Source, source.Schema)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	importer := &CostImporter{repository: r, source: source, binding: binding, digest: hex.EncodeToString(digest[:]), organization: tracker.OrganizationID(source.Source.OrganizationID)}
	if err := importer.validateSource(ctx); err != nil {
		return nil, err
	}
	return importer, nil
}

func (i *CostImporter) sourceTable() string {
	return pq.QuoteIdentifier(i.source.Source.Schema) + `."usage_cost_observations"`
}

func (i *CostImporter) validateSource(ctx context.Context) error {
	tx, err := i.sourceTransaction(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT attname,atttypid='text'::regtype FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped ORDER BY attnum`, i.sourceTable())
	if err != nil {
		return err
	}
	defer rows.Close()
	definition := i.source.Catalog.Tables["hub/usage_cost_observations"]
	count := 0
	for rows.Next() {
		var name string
		var textType bool
		if err := rows.Scan(&name, &textType); err != nil {
			return err
		}
		count++
		mapping, ok := definition.Columns[name]
		if !ok || i.source.Schema["usage_cost_observations"][name] != mapping.Source || !textType {
			return storagecatalog.ErrUnmapped
		}
		if mapping.Disposition == "pending" {
			return storagecatalog.ErrPending
		}
		if mapping.Disposition == "transform" && mapping.Transform != "cost-observation" {
			return storagecatalog.ErrUnmapped
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != len(costImportColumns) {
		return storagecatalog.ErrUnmapped
	}
	if err := rows.Close(); err != nil {
		return err
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
WHERE k.n<=i.indnkeyatts AND k.coll<>'"C"'::regcollation))`, i.sourceTable(), pq.Array(costImportColumns[:7])).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrCostImport
	}
	return tx.Commit()
}

func (i *CostImporter) sourceTransaction(ctx context.Context) (*sql.Tx, error) {
	tx, err := i.source.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "SET TRANSACTION SNAPSHOT "+pq.QuoteLiteral(i.source.SnapshotID)); err != nil {
		return nil, errors.Join(ErrCostImport, err, tx.Rollback())
	}
	return tx, nil
}
