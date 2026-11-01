package accessstore

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func VerifySecret(keys *hubsecrets.Keyring, secret Secret) error {
	return keys.Check(secret.Envelope, SecretAAD(secret.OrganizationID, secret.ProjectID, secret.Kind))
}

func OpenSecret(keys *hubsecrets.Keyring, secret Secret) ([]byte, error) {
	return keys.Open(secret.Envelope, SecretAAD(secret.OrganizationID, secret.ProjectID, secret.Kind))
}

func VerifyTenantSecrets(ctx context.Context, read func(context.Context, func(Reader) error) error, keys *hubsecrets.Keyring) error {
	var project tracker.ProjectID
	var kind string
	for {
		var page []Secret
		if err := read(ctx, func(r Reader) error { var err error; page, err = r.AccessSecrets(ctx, project, kind, 200); return err }); err != nil {
			return fmt.Errorf("read tenant secrets: %w", err)
		}
		for _, secret := range page {
			if err := VerifySecret(keys, secret); err != nil {
				return fmt.Errorf("verify tenant secret: %w", err)
			}
			project, kind = secret.ProjectID, secret.Kind
		}
		if len(page) < 200 {
			return nil
		}
	}
}
