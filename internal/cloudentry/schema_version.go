package cloudentry

import "github.com/digitaldrywood/detent/internal/hubserver/postgres"

type SchemaVersions struct {
	Registry            int64
	Auth                int64
	RegistryFingerprint string
	AuthFingerprint     string
}

func LatestSchemaVersions() (SchemaVersions, error) {
	version, err := postgres.LatestSchemaVersion()
	if err != nil {
		return SchemaVersions{}, err
	}
	fingerprint, err := postgres.LatestSchemaFingerprint()
	if err != nil {
		return SchemaVersions{}, err
	}
	return SchemaVersions{Registry: version, Auth: version, RegistryFingerprint: fingerprint, AuthFingerprint: fingerprint}, nil
}
