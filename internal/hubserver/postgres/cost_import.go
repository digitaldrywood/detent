package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CostImportSource struct {
	BundlePath string
	neutral    *VerifiedNeutralImport
	CopyPath   string
	Catalog    storagecatalog.Catalog
	Source     storagecatalog.Source
	Schema     storagecatalog.Schema
	Objects    attachment.DetailStore
	copyTx     *sql.Tx
	snapshotID string
}

type costImporter struct {
	repository   *Repository
	source       CostImportSource
	binding      *storagecatalog.Binding
	digest       string
	organization tracker.OrganizationID
}

func (r *Repository) newCostImporter(ctx context.Context, source CostImportSource) (*costImporter, error) {
	if source.copyTx == nil && source.neutral == nil || source.Objects == nil || source.Source.Store != "hub" || source.snapshotID == "" || len(source.snapshotID) > 200 || strings.TrimSpace(source.Source.OrganizationID) != source.Source.OrganizationID {
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
	importer := &costImporter{repository: r, source: source, binding: binding, digest: hex.EncodeToString(digest[:]), organization: tracker.OrganizationID(source.Source.OrganizationID)}
	if err := importer.validateSource(ctx); err != nil {
		return nil, err
	}
	return importer, nil
}

func (i *costImporter) validateSource(ctx context.Context) error {
	if i.source.neutral != nil {
		if err := i.source.neutral.ValidateCatalogTable(i.source.Catalog, i.source.Schema, "usage_cost_observations", costImportColumns[:7]); err != nil {
			return err
		}
		for _, name := range costImportColumns {
			column := i.source.Catalog.Tables["hub/usage_cost_observations"].Columns[name]
			if column.Disposition == "pending" {
				return storagecatalog.ErrPending
			}
			if column.Disposition == "transform" && column.Transform != "cost-observation" {
				return storagecatalog.ErrUnmapped
			}
		}
		return nil
	}
	rows, err := i.source.copyTx.QueryContext(ctx, `SELECT cid,name,type,"notnull",dflt_value,pk,hidden FROM pragma_table_xinfo(?) ORDER BY cid`, "usage_cost_observations")
	if err != nil {
		return err
	}
	defer rows.Close()
	definition := i.source.Catalog.Tables["hub/usage_cost_observations"]
	count := 0
	primary := make([]string, 7)
	for rows.Next() {
		var name string
		var ordinal, notNull, primaryKey, hidden int
		var columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&ordinal, &name, &columnType, &notNull, &defaultValue, &primaryKey, &hidden); err != nil {
			return err
		}
		var actualDefault any
		if defaultValue.Valid {
			actualDefault = defaultValue.String
		}
		actual, err := json.Marshal([]any{ordinal, name, columnType, notNull, actualDefault, primaryKey, hidden})
		if err != nil {
			return err
		}
		count++
		mapping, ok := definition.Columns[name]
		if !ok || i.source.Schema["usage_cost_observations"][name] != mapping.Source || !sameSourceSignature(actual, mapping.Source) {
			return storagecatalog.ErrUnmapped
		}
		if primaryKey > 0 {
			if primaryKey > len(primary) {
				return ErrCostImport
			}
			primary[primaryKey-1] = name
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
	if err := rows.Close(); err != nil {
		return err
	}
	if count != len(costImportColumns) || !slices.Equal(primary, costImportColumns[:7]) {
		return storagecatalog.ErrUnmapped
	}
	return validateImportCopyPrimaryIndex(ctx, i.source.copyTx, "usage_cost_observations", costImportColumns[:7])
}

func schemaFromCatalog(catalog storagecatalog.Catalog) storagecatalog.Schema {
	schema := storagecatalog.Schema{}
	for name, table := range catalog.Tables {
		name, ok := strings.CutPrefix(name, "hub/")
		if !ok {
			continue
		}
		schema[name] = make(map[string]string, len(table.Columns))
		for column, definition := range table.Columns {
			schema[name][column] = definition.Source
		}
	}
	return schema
}

func sameSourceSignature(actual []byte, expected string) bool {
	var left, right any
	if json.Unmarshal(actual, &left) != nil || json.Unmarshal([]byte(expected), &right) != nil {
		return false
	}
	actualFields, actualOK := left.([]any)
	expectedFields, expectedOK := right.([]any)
	return actualOK && expectedOK && len(actualFields) == 7 && len(expectedFields) == 7 && reflect.DeepEqual(actualFields[1:], expectedFields[1:])
}

func (r *Repository) ImportCostCopy(ctx context.Context, source CostImportSource) (CostImportProgress, error) {
	var progress CostImportProgress
	if (source.CopyPath == "") == (source.BundlePath == "") || source.Objects == nil || source.Source.Store != "hub" || source.Source.OrganizationID == "" {
		return progress, ErrCostImport
	}
	if source.Schema == nil {
		source.Schema = schemaFromCatalog(source.Catalog)
	}
	source.Source.Schema = "hub_copy"
	if _, err := source.Catalog.Bind(source.Source, source.Schema); err != nil {
		return progress, err
	}
	organization := tracker.OrganizationID(source.Source.OrganizationID)
	if source.BundlePath != "" {
		err := ReadNeutralImportBundle(ctx, source.BundlePath, organization, func(bundle *VerifiedNeutralImport) error {
			source.neutral, source.snapshotID = bundle, bundle.Identity().SourceSHA256
			importer, err := r.newCostImporter(ctx, source)
			if err != nil {
				return err
			}
			if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
				return err
			}
			for {
				progress, err = importer.step(ctx, costImportPageLimit)
				if err != nil || progress.Done {
					return err
				}
			}
		})
		return progress, err
	}
	err := ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, digest string) error {
		if err := r.PinCoreImportSource(ctx, organization, digest, source.Objects); err != nil {
			return err
		}
		source.copyTx, source.snapshotID = tx, digest
		importer, err := r.newCostImporter(ctx, source)
		if err != nil {
			return err
		}
		for {
			progress, err = importer.step(ctx, costImportPageLimit)
			if err != nil || progress.Done {
				return err
			}
		}
	})
	return progress, err
}
