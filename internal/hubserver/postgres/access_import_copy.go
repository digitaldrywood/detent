package postgres

import (
	"context"
	"database/sql"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AccessCopyImportSource struct {
	BundlePath     string
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
	if source.CopyPath != "" {
		return 0, ErrNeutralImport

	}

	if source.BundlePath == "" || source.OrganizationID == "" || source.Objects == nil {
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
	run := func(input AccessImportSource) error {
		input.Catalog = source.Catalog
		input.Source = storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: source.Catalog.Version, OrganizationID: string(source.OrganizationID)}
		input.Schema, input.Objects = schema, source.Objects
		importer, err := r.NewAccessImporter(ctx, input)
		if err != nil {
			return err
		}

		if err := importer.pinNeutralAccessSource(ctx); err != nil {
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
	}

	err := ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *VerifiedNeutralImport) error {
		return run(AccessImportSource{neutral: bundle, SnapshotID: bundle.Identity().SourceSHA256})
	})
	return processed, err
}

func (i *AccessImporter) sourceCollation() string {
	return `"C"`
}

func (i *AccessImporter) querySource(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return i.source.DB.QueryContext(ctx, query, args...)
}
