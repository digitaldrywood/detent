package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

type cloudPlatformImportConfig struct {
	postgresDSN string
	pool        platformstore.PoolConfig
	signingKey  ed25519.PrivateKey
	keys        *hubsecrets.Keyring
	attachments attachment.Config
}

func newCloudPlatformImportCommand(lookupEnv func(string) string) *cobra.Command {
	var configPath, registryPath, authPath, repositoryRoot string
	var limit int
	cmd := &cobra.Command{
		Use:          "import-platform-postgres",
		Short:        "Import sealed registry and auth copies into PostgreSQL",
		Long:         "Run the bounded, resumable platform import from direct registry.db and auth.db copies into an already migrated, empty PostgreSQL platform target. The copies must come from a stopped shared entry, be read-only, and have no WAL, shared-memory or journal sidecars. This command does not start the Cloud service or modify the source copies.",
		Example:      "detent cloud import-platform-postgres --entry-config /etc/detent/cloud.yaml --registry-copy /secure-copy/registry.db --auth-copy /secure-copy/auth.db --repository-root /opt/detent/source",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			output, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			return runCloudPlatformImport(cmd.Context(), output, configPath, registryPath, authPath, repositoryRoot, limit, lookupEnv)
		},
	}
	cmd.Flags().StringVar(&configPath, "entry-config", "", "shared entry PostgreSQL and archive configuration")
	cmd.Flags().StringVar(&registryPath, "registry-copy", "", "read-only direct copy of registry.db without SQLite sidecars")
	cmd.Flags().StringVar(&authPath, "auth-copy", "", "read-only direct copy of auth.db without SQLite sidecars")
	cmd.Flags().StringVar(&repositoryRoot, "repository-root", "", "checked-out Detent source tree for the mapping catalog")
	cmd.Flags().IntVar(&limit, "batch-size", platformstore.ImportLimit, "maximum source rows per target transaction")
	for _, flag := range []string{"entry-config", "registry-copy", "auth-copy", "repository-root"} {
		if err := cmd.MarkFlagRequired(flag); err != nil {
			panic(err)
		}
	}
	return cmd
}

func runCloudPlatformImport(ctx context.Context, output CommandOutput, configPath, registryPath, authPath, repositoryRoot string, limit int, lookupEnv func(string) string) (resultErr error) {
	config, err := readCloudPlatformImportConfig(configPath, lookupEnv)
	if err != nil {
		return err
	}
	catalog, err := readPlatformImportCatalog(repositoryRoot)
	if err != nil {
		return err
	}
	copies, err := platformstore.OpenSQLiteCopySource(ctx, registryPath, authPath, catalog)
	if err != nil {
		return fmt.Errorf("open sealed platform source copies: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, copies.Close()) }()
	objects, err := attachment.NewStorage(ctx, config.attachments, nil)
	if err != nil {
		return fmt.Errorf("open platform archive storage: %w", err)
	}
	pool, err := sql.Open("postgres", config.postgresDSN)
	if err != nil {
		return errors.New("open PostgreSQL platform target")
	}
	repository, err := platformstore.New(ctx, pool, platformstore.Config{Pool: config.pool, SigningKey: config.signingKey, Keys: config.keys})
	if err != nil {
		return errors.Join(fmt.Errorf("open PostgreSQL platform target: %w", err), pool.Close())
	}
	defer func() { resultErr = errors.Join(resultErr, repository.Close()) }()
	source := copies.ImportSource()
	publicKey, ok := config.signingKey.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("shared entry signing key has an invalid public key")
	}
	source.SigningKey = publicKey
	source.KeyVersions = config.keys.Versions()
	source.Objects = objects
	importer, err := repository.NewImporter(ctx, source)
	if err != nil {
		return fmt.Errorf("prepare PostgreSQL platform import: %w", err)
	}
	var processed int64
	for {
		progress, err := importer.Step(ctx, limit)
		if err != nil {
			return fmt.Errorf("import PostgreSQL platform data: %w", err)
		}
		processed = progress.Processed
		if progress.Done {
			break
		}
	}
	return output.Write(func(writer io.Writer) error {
		_, err := fmt.Fprintf(writer, "PostgreSQL platform import complete: processed=%d\n", processed)
		return err
	}, struct {
		Done      bool  `json:"done"`
		Processed int64 `json:"processed"`
	}{Done: true, Processed: processed})
}

func readCloudPlatformImportConfig(path string, lookupEnv func(string) string) (cloudPlatformImportConfig, error) {
	if lookupEnv == nil {
		return cloudPlatformImportConfig{}, errors.New("platform import environment is unavailable")
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 64*1024 {
		return cloudPlatformImportConfig{}, errors.New("shared entry configuration is unavailable or too large")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var file cloudFileConfig
	if err := decoder.Decode(&file); err != nil {
		return cloudPlatformImportConfig{}, errors.New("shared entry configuration is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return cloudPlatformImportConfig{}, errors.New("shared entry configuration must contain one document")
	}
	if file.Storage.Backend != "postgres" || !validEnvName(file.Storage.PostgresDSNEnv) || strings.TrimSpace(lookupEnv(file.Storage.PostgresDSNEnv)) == "" {
		return cloudPlatformImportConfig{}, errors.New("platform import requires the configured PostgreSQL storage backend")
	}
	if file.Assertion.SigningKeyEnv == "" {
		file.Assertion.SigningKeyEnv = "DETENT_CLOUD_ASSERTION_KEY"
	}
	if !validEnvName(file.Assertion.SigningKeyEnv) {
		return cloudPlatformImportConfig{}, errors.New("shared entry signing key environment variable name is invalid")
	}
	signingKey, err := cloudassert.ParsePrivateKey(lookupEnv(file.Assertion.SigningKeyEnv))
	if err != nil {
		return cloudPlatformImportConfig{}, errors.New("shared entry signing key is unavailable or invalid")
	}
	if file.Attachments == nil {
		return cloudPlatformImportConfig{}, errors.New("platform import requires immutable archive storage")
	}
	attachments := *file.Attachments
	attachments.AccessKeyID = lookupEnv("DETENT_ATTACHMENTS_ACCESS_KEY_ID")
	attachments.SecretAccessKey = lookupEnv("DETENT_ATTACHMENTS_SECRET_ACCESS_KEY")
	keys, err := hubsecrets.FromEnvironment(lookupEnv)
	if err != nil {
		return cloudPlatformImportConfig{}, err
	}
	return cloudPlatformImportConfig{
		postgresDSN: lookupEnv(file.Storage.PostgresDSNEnv),
		pool: platformstore.PoolConfig{
			MaxConnections:        file.Storage.MaxConnections,
			ServingConnections:    file.Storage.ServingConnections,
			BackgroundConnections: file.Storage.BackgroundConnections,
		},
		signingKey:  signingKey,
		keys:        keys,
		attachments: attachments,
	}, nil
}

func readPlatformImportCatalog(repositoryRoot string) (storagecatalog.Catalog, error) {
	absolute, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return storagecatalog.Catalog{}, err
	}
	files := os.DirFS(absolute)
	catalog, err := storagecatalog.Read(files, "tools/storageinventory/mappings/v1.json")
	if err != nil {
		return storagecatalog.Catalog{}, fmt.Errorf("read platform import mapping catalog: %w", err)
	}
	if err := catalog.ValidateInventory(files); err != nil {
		return storagecatalog.Catalog{}, fmt.Errorf("validate platform import mapping catalog: %w", err)
	}
	return catalog, nil
}
