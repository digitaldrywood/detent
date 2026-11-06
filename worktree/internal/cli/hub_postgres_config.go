package cli

import (
	"errors"
	"strings"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type hubPostgresFileConfig struct {
	Host                  string `yaml:"host"`
	Port                  int    `yaml:"port"`
	Database              string `yaml:"database"`
	User                  string `yaml:"user"`
	PasswordEnv           string `yaml:"password_env"`
	SSLMode               string `yaml:"sslmode"`
	MaxConnections        int    `yaml:"max_connections"`
	ServingConnections    int    `yaml:"serving_connections"`
	BackgroundConnections int    `yaml:"background_connections"`
}

func (c *hubPostgresFileConfig) runtime(lookupEnv func(string) string) (*postgres.Config, error) {
	if c == nil {
		return nil, errors.New("postgres runtime configuration is required")
	}
	if strings.TrimSpace(c.Host) == "" || c.Port < 1 || c.Port > 65535 || strings.TrimSpace(c.Database) == "" || strings.TrimSpace(c.User) == "" || !validEnvName(c.PasswordEnv) || strings.TrimSpace(c.SSLMode) == "" || lookupEnv(c.PasswordEnv) == "" || c.MaxConnections < 2 || c.ServingConnections < 1 || c.BackgroundConnections < 1 || c.ServingConnections+c.BackgroundConnections > c.MaxConnections {
		return nil, errors.New("postgres runtime configuration is incomplete or invalid")
	}
	return &postgres.Config{
		Connection: pq.Config{Host: c.Host, Port: uint16(c.Port), Database: c.Database, User: c.User, Password: lookupEnv(c.PasswordEnv), SSLMode: pq.SSLMode(c.SSLMode)},
		Pool:       postgres.PoolConfig{MaxConnections: c.MaxConnections, ServingConnections: c.ServingConnections, BackgroundConnections: c.BackgroundConnections},
	}, nil
}
