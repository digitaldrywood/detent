package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AccessCopyImportSource struct {
	CopyPath       string
	OrganizationID tracker.OrganizationID
	Catalog        storagecatalog.Catalog
	Objects        attachment.DetailStore
}

func (r *Repository) ImportAccessCopy(ctx context.Context, source AccessCopyImportSource) (int64, error) {
	return r.importAccessCopy(ctx, source, false)
}

func (r *Repository) InitializeAccessCopyOrganization(ctx context.Context, source AccessCopyImportSource) error {
	_, err := r.importAccessCopy(ctx, source, true)
	return err
}

func (r *Repository) importAccessCopy(ctx context.Context, source AccessCopyImportSource, organizationOnly bool) (int64, error) {
	if source.CopyPath == "" || source.OrganizationID == "" || source.Objects == nil {
		return 0, ErrAccessImport
	}
	if err := source.Catalog.Validate(); err != nil {
		return 0, err
	}
	schema := storagecatalog.Schema{}
	for name, table := range source.Catalog.Tables {
		name, ok := strings.CutPrefix(name, "hub/")
		if !ok {
			continue
		}
		schema[name] = make(map[string]string, len(table.Columns))
		for column, definition := range table.Columns {
			schema[name][column] = definition.Source
		}
	}
	var processed int64
	err := ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, digest string) error {
		if err := r.PinCoreImportSource(ctx, source.OrganizationID, digest, source.Objects); err != nil {
			return err
		}
		importer, err := r.NewAccessImporter(ctx, AccessImportSource{
			copyTx:  tx,
			Catalog: source.Catalog,
			Source:  storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: source.Catalog.Version, OrganizationID: string(source.OrganizationID)},
			Schema:  schema, SnapshotID: digest, Objects: source.Objects,
		})
		if err != nil {
			return err
		}
		if organizationOnly {
			checkpoint, err := importer.start(ctx)
			if err != nil {
				return err
			}
			if checkpoint.stage > 0 {
				return nil
			}
		}
		for {
			progress, err := importer.Step(ctx, AccessImportLimit)
			if err != nil {
				return err
			}
			processed = progress.Processed
			if progress.Done || organizationOnly {
				return nil
			}
		}
	})
	return processed, err
}

func (i *AccessImporter) sourceCollation() string {
	if i.source.copyTx != nil {
		return "BINARY"
	}
	return `"C"`
}

func (i *AccessImporter) querySource(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if i.source.copyTx != nil {
		return i.source.copyTx.QueryContext(ctx, query, args...)
	}
	return i.source.DB.QueryContext(ctx, query, args...)
}

func (i *AccessImporter) validateCopyTable(ctx context.Context, table string) error {
	rows, err := i.querySource(ctx, `SELECT cid,name,type,"notnull",dflt_value,pk,hidden FROM pragma_table_xinfo(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	expected := i.source.Schema[table]
	seen := make(map[string]bool, len(expected))
	for rows.Next() {
		var ordinal, notNull, primary, hidden int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&ordinal, &name, &columnType, &notNull, &defaultValue, &primary, &hidden); err != nil {
			return err
		}
		var actualDefault any
		if defaultValue.Valid {
			actualDefault = defaultValue.String
		}
		actual, err := json.Marshal([]any{ordinal, name, columnType, notNull, actualDefault, primary, hidden})
		if err != nil {
			return err
		}
		var left, right any
		if json.Unmarshal(actual, &left) != nil || json.Unmarshal([]byte(expected[name]), &right) != nil || !reflect.DeepEqual(left, right) {
			return storagecatalog.ErrUnmapped
		}
		seen[name] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(seen) != len(expected) || len(seen) == 0 {
		return storagecatalog.ErrUnmapped
	}
	return nil
}
