package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func (i *AccessImporter) pinNeutralAccessSource(ctx context.Context) error {
	source := i.source.neutral
	if err := i.repository.PinCoreImportSource(ctx, i.organization, source.Identity().SourceSHA256, i.source.Objects); err != nil {
		return err
	}
	manifest, err := json.Marshal(source.manifest)
	if err != nil {
		return err
	}
	schema, err := json.Marshal(source.Schema())
	if err != nil {
		return err
	}
	receipt, err := eventstore.NewContent(i.source.Objects).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: i.organization, ActorID: "production-import", Operation: "neutral-source", CommandKey: "source", RequestHash: source.Digest()}, manifest)
	if err != nil {
		return err
	}
	rows, err := i.readNeutralAccessPage(ctx, accessImportTable{name: "organizations", keys: []string{"id"}}, accessImportCheckpoint{}, 2)
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return storagecatalog.ErrOrganization
	}
	err = i.repository.transaction(ctx, i.organization, corestore.Background, false, func(s *session) error {
		if err := i.validateOrganization(ctx, s, rows[0]); err != nil {
			return err
		}
		identity := source.Identity()
		if _, err := s.BindNeutralImportSource(ctx, BindNeutralImportSourceInput{OrganizationID: i.organization, SourceID: identity.SourceID, SchemaVersion: int64(identity.SchemaVersion), Checksum: source.Digest(), SchemaDigest: artifact.Digest(schema), VerifiedAt: time.Now().UTC()}); err != nil {
			return err
		}
		_, err := s.PutReceipt(ctx, receipt)
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return mutation.ErrConflict
	}
	return err
}

func (i *AccessImporter) validateNeutralAccessOrganization(ctx context.Context) error {
	table, err := i.source.neutral.table("organizations")
	if err != nil || len(table.Pages) != 1 || table.Pages[0].Rows != 1 {
		return storagecatalog.ErrOrganization
	}
	rows, err := i.source.neutral.Page(ctx, "organizations", nil, 2)
	if err != nil {
		return err
	}
	if len(rows) != 1 || rows[0].Values["id"].Null || string(rows[0].Values["id"].Data) != string(i.organization) {
		return storagecatalog.ErrOrganization
	}
	return nil
}

func (i *AccessImporter) readNeutralAccessPage(ctx context.Context, table accessImportTable, checkpoint accessImportCheckpoint, limit int) ([]accessImportRow, error) {
	var after []string
	if checkpoint.hasAfter {
		after = checkpoint.after[:len(table.keys)]
	}
	rows, err := i.source.neutral.Page(ctx, table.name, after, limit)
	if err != nil {
		return nil, err
	}
	page := make([]accessImportRow, 0, len(rows))
	for _, source := range rows {
		row := accessImportRow{raw: source.Values, values: make(map[string]storagecatalog.Value)}
		for name, cell := range source.Values {
			disposition := i.source.Catalog.Tables["hub/"+table.name].Columns[name].Disposition
			if disposition == "column" || disposition == "object" {
				value, err := i.source.Catalog.Decode("hub/"+table.name, name, cell)
				if err != nil {
					return nil, err
				}
				row.values[name] = value
			}
		}
		if _, err := i.binding.ValidateMappedRow(table.name, source.Values); err != nil {
			return nil, err
		}
		for index, key := range table.keys {
			cell := source.Values[key]
			if cell.Null || cell.Encoding != "utf8" || strings.IndexByte(string(cell.Data), 0) >= 0 {
				return nil, storagecatalog.ErrValue
			}
			row.keyValues[index] = string(cell.Data)
		}
		page = append(page, row)
	}
	return page, nil
}
