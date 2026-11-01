package cloudentry

import (
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/schemafingerprint"
)

type SchemaVersions struct {
	Registry            int64
	Auth                int64
	RegistryFingerprint string
	AuthFingerprint     string
}

func LatestSchemaVersions() (SchemaVersions, error) {
	var versions SchemaVersions
	for directory, target := range map[string]struct {
		version     *int64
		fingerprint *string
	}{"registry": {version: &versions.Registry, fingerprint: &versions.RegistryFingerprint}, "auth": {version: &versions.Auth, fingerprint: &versions.AuthFingerprint}} {
		entries, err := fs.ReadDir(migrationFiles, "migrations/"+directory)
		if err != nil {
			return SchemaVersions{}, err
		}
		ids := make([]int64, 0, len(entries))
		for _, entry := range entries {
			prefix, _, _ := strings.Cut(entry.Name(), "_")
			version, err := strconv.ParseInt(prefix, 10, 64)
			if err != nil {
				return SchemaVersions{}, fmt.Errorf("parse %s migration version: %w", directory, err)
			}
			ids = append(ids, version)
			*target.version = max(*target.version, version)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		*target.fingerprint = schemafingerprint.Versions(ids)
	}
	return versions, nil
}
