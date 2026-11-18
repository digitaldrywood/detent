package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type hubPostgresFileConfig struct {
	Host                  string `yaml:"host"`
	Port                  int    `yaml:"port"`
	Database              string `yaml:"database"`
	User                  string `yaml:"user"`
	PasswordEnv           string `yaml:"password_env"`
	SSLMode               string `yaml:"sslmode"`
	DefaultQueryExecMode  string `yaml:"default_query_exec_mode,omitempty"`
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
		Connection: pgtransport.Connection{Host: c.Host, Port: uint16(c.Port), Database: c.Database, User: c.User, Password: lookupEnv(c.PasswordEnv), SSLMode: c.SSLMode, DefaultQueryExecMode: c.DefaultQueryExecMode},
		Pool:       postgres.PoolConfig{MaxConnections: c.MaxConnections, ServingConnections: c.ServingConnections, BackgroundConnections: c.BackgroundConnections},
	}, nil
}

func readHubServeConfig(path string, lookupEnv func(string) string) (_ *hostedplans.HostedConfig, _ *postgres.Config, resultErr error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, errors.New("hub configuration could not be opened")
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("hub configuration could not be closed"))
		}
	}()
	const maxConfigBytes = 128 * 1024
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return nil, nil, errors.New("hub configuration could not be read within the size limit")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("hub configuration is invalid")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("hub configuration must contain one document")
	}
	fields := document.Content[0].Content
	if len(fields) == 2 && fields[0].Kind == yaml.ScalarNode && fields[0].Value == "postgres" {
		var config hostedFileConfig
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&config); err != nil {
			return nil, nil, errors.New("hub PostgreSQL configuration is invalid")
		}
		pg, err := config.Postgres.runtime(lookupEnv)
		return nil, pg, err
	}
	hosted, err := parseHostedConfig(bytes.NewReader(data), lookupEnv)
	if err != nil {
		return nil, nil, err
	}
	return hosted, hosted.Postgres, nil
}
