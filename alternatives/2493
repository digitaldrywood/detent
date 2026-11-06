package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CoreHubIdentityImportSource struct {
	CopyPath       string
	OrganizationID tracker.OrganizationID
	Objects        attachment.DetailStore
}

func (r *Repository) ImportCoreHubIdentity(ctx context.Context, source CoreHubIdentityImportSource) (int64, error) {
	if source.CopyPath == "" || source.OrganizationID == "" || source.Objects == nil {
		return 0, errors.New("invalid core Hub identity import source")
	}
	var imported int64
	err := ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		if err := r.PinCoreImportSource(ctx, source.OrganizationID, copyHash, source.Objects); err != nil {
			return err
		}
		var identity accessstore.HubIdentity
		identity.OrganizationID = source.OrganizationID
		if err := tx.QueryRowContext(ctx, `SELECT id,cursor_key FROM hub_identity`).Scan(&identity.ID, &identity.CursorKey); err != nil {
			return fmt.Errorf("read source Hub identity: %w", err)
		}
		if !validSourceHubIdentity(identity.ID) || len(identity.CursorKey) != 32 {
			return errors.New("invalid source Hub identity")
		}
		var rows int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM hub_identity`).Scan(&rows); err != nil {
			return fmt.Errorf("count source Hub identities: %w", err)
		}
		if rows != 1 {
			return errors.New("source Hub identity is not a singleton")
		}
		if err := r.WriteTenant(ctx, source.OrganizationID, corestore.Background, func(writer tenantstore.Writer) error {
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
		}); err != nil {
			return err
		}
		imported = 1
		return nil
	})
	return imported, err
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
