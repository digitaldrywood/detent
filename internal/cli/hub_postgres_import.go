package cli

import (
	"bytes"
	"errors"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func readTenantImportConfig(path string, lookupEnv func(string) string) (*postgres.Config, attachment.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 64*1024 {
		return nil, attachment.Config{}, errors.New("tenant import configuration is unavailable or too large")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var file cloudFileConfig
	if err := decoder.Decode(&file); err != nil {
		return nil, attachment.Config{}, errors.New("tenant import configuration is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) || file.Storage.Backend != "postgres" || file.TenantPostgres == nil || file.Attachments == nil || lookupEnv == nil {
		return nil, attachment.Config{}, errors.New("tenant import requires PostgreSQL tenant and archive configuration")
	}
	config, err := file.TenantPostgres.runtime(lookupEnv)
	if err != nil {
		return nil, attachment.Config{}, err
	}
	archive := *file.Attachments
	archive.AccessKeyID = lookupEnv("DETENT_ATTACHMENTS_ACCESS_KEY_ID")
	archive.SecretAccessKey = lookupEnv("DETENT_ATTACHMENTS_SECRET_ACCESS_KEY")
	return config, archive, nil
}
