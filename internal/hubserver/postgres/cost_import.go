package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
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
	if source.neutral == nil || source.Objects == nil || source.Source.Store != "hub" || source.snapshotID == "" || len(source.snapshotID) > 200 || strings.TrimSpace(source.Source.OrganizationID) != source.Source.OrganizationID {
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
	if source.CopyPath != "" {
		return CostImportProgress{}, ErrNeutralImport
	}

	var progress CostImportProgress
	if source.BundlePath == "" || source.Objects == nil || source.Source.Store != "hub" || source.Source.OrganizationID == "" {
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
