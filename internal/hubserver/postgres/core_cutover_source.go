package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func VerifyNeutralCoreCutover(ctx context.Context, directory string, organization tracker.OrganizationID) error {
	return ReadNeutralImportBundle(ctx, directory, organization, func(source *VerifiedNeutralImport) error {
		for _, table := range []string{"artifact_services", "artifact_grants", "artifact_references"} {
			rows, err := source.Page(ctx, table, nil, 1)
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				return errors.New("production source contains artifact records requiring import before cutover")
			}
		}
		return nil
	})
}
