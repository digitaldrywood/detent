package postgres

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CoreHubIdentityImportSource struct {
	BundlePath     string
	CopyPath       string
	OrganizationID tracker.OrganizationID
	Objects        attachment.DetailStore
}

func (r *Repository) ImportCoreHubIdentity(ctx context.Context, source CoreHubIdentityImportSource) (int64, error) {
	if source.CopyPath != "" {
		return 0, ErrNeutralImport
	}

	if source.BundlePath == "" || source.OrganizationID == "" || source.Objects == nil {
		return 0, errors.New("invalid core Hub identity import source")
	}
	var imported int64

	err := ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *VerifiedNeutralImport) error {
		identity, err := neutralHubIdentity(ctx, bundle)
		if err != nil {
			return err
		}
		if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		if err := r.importHubIdentity(ctx, identity); err != nil {
			return err
		}
		imported = 1
		return nil
	})
	return imported, err
}

func (r *Repository) importHubIdentity(ctx context.Context, identity accessstore.HubIdentity) error {
	return r.WriteTenant(ctx, identity.OrganizationID, corestore.Background, func(writer tenantstore.Writer) error {
		current, err := writer.HubIdentity(ctx)
		if err == nil {
			if current.ID != identity.ID || !bytes.Equal(current.CursorKey, identity.CursorKey) {
				return corestore.ErrRevisionConflict
			}
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return writer.CreateHubIdentity(ctx, identity)
	})
}

func neutralHubIdentity(ctx context.Context, bundle *VerifiedNeutralImport) (accessstore.HubIdentity, error) {
	table, err := bundle.table("hub_identity")
	if err != nil || len(table.Columns) != 2 || len(table.Pages) != 1 || table.Pages[0].Rows != 1 || !slices.Equal(table.Key, []NeutralImportKey{{Column: "id", Kind: "text"}}) || !sameSourceSignature([]byte(table.Columns["id"]), `[0,"id","TEXT",0,null,1,0]`) || !sameSourceSignature([]byte(table.Columns["cursor_key"]), `[1,"cursor_key","BLOB",1,null,0,0]`) {
		return accessstore.HubIdentity{}, ErrNeutralImport
	}
	rows, err := bundle.Page(ctx, "hub_identity", nil, 2)
	if err != nil {
		return accessstore.HubIdentity{}, err
	}
	if len(rows) != 1 {
		return accessstore.HubIdentity{}, ErrNeutralImport
	}
	identity := accessstore.HubIdentity{OrganizationID: bundle.Identity().OrganizationID}
	if err := rows[0].Decode(map[string]any{"id": &identity.ID}); err != nil {
		return accessstore.HubIdentity{}, err
	}
	cell := rows[0].Values["cursor_key"]
	if cell.Null {
		return accessstore.HubIdentity{}, storagecatalog.ErrValue
	}
	switch cell.Encoding {
	case "binary":
		identity.CursorKey = bytes.Clone(cell.Data)
	case "hex":
		identity.CursorKey, err = hex.DecodeString(string(cell.Data))
	case "base64":
		identity.CursorKey, err = base64.StdEncoding.Strict().DecodeString(string(cell.Data))
		if err == nil && base64.StdEncoding.EncodeToString(identity.CursorKey) != string(cell.Data) {
			err = storagecatalog.ErrValue
		}
	default:
		err = storagecatalog.ErrValue
	}
	if err != nil || !validSourceHubIdentity(identity.ID) || len(identity.CursorKey) != 32 {
		return accessstore.HubIdentity{}, storagecatalog.ErrValue
	}
	return identity, nil
}

func validSourceHubIdentity(id string) bool {
	if len(id) != 36 || !strings.HasPrefix(id, "hub_") {
		return false
	}
	for _, value := range id[4:] {
		if value < '0' || value > '9' {
			if value < 'a' || value > 'f' {
				return false
			}
		}
	}
	return true
}
