package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"github.com/spf13/cobra"
)

func newHubPostgresMigrationCommand(lookupEnv func(string) string, migrate func(context.Context, pq.Config) error) *cobra.Command {
	var host, database, user, passwordEnv, sslMode string
	var port int
	cmd := &cobra.Command{
		Use:          "migrate-postgres",
		Short:        "Apply PostgreSQL schema migrations with a separate migration login",
		Example:      "detent hub migrate-postgres --host postgres.example.com --port 5432 --database detent --user detent_migration_login --password-env DETENT_MIGRATION_PASSWORD --sslmode verify-full",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(host) == "" || strings.TrimSpace(database) == "" || strings.TrimSpace(user) == "" || port < 1 || port > 65535 || !validEnvName(passwordEnv) || lookupEnv(passwordEnv) == "" {
				return errors.New("postgres migration requires explicit host, port, database, user and password environment variable")
			}
			switch sslMode {
			case "disable", "require", "verify-ca", "verify-full":
			default:
				return errors.New("postgres migration requires an explicit supported TLS mode")
			}
			connection := pq.Config{Host: host, Port: uint16(port), Database: database, User: user, Password: lookupEnv(passwordEnv), SSLMode: pq.SSLMode(sslMode)}
			if err := migrate(cmd.Context(), connection); err != nil {
				return fmt.Errorf("migrate postgres schema: %w", err)
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "PostgreSQL schema migrations applied")
			return err
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "PostgreSQL host")
	cmd.Flags().IntVar(&port, "port", 0, "PostgreSQL port")
	cmd.Flags().StringVar(&database, "database", "", "PostgreSQL database")
	cmd.Flags().StringVar(&user, "user", "", "separate migration login")
	cmd.Flags().StringVar(&passwordEnv, "password-env", "", "environment variable containing the migration password")
	cmd.Flags().StringVar(&sslMode, "sslmode", "", "explicit PostgreSQL TLS mode")
	return cmd
}
