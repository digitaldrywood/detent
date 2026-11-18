package pgtransport

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Connection struct {
	Host     string
	Port     uint16
	Database string
	User     string
	Password string
	SSLMode  string
}

var ErrConnectionConfig = errors.New("PostgreSQL requires explicit connection settings without credential discovery")

func (c Connection) Clone() Connection { return c }
func (c Connection) PoolConfig() (*pgxpool.Config, error) {
	if strings.TrimSpace(c.Host) == "" || c.Port == 0 || strings.TrimSpace(c.Database) == "" || strings.TrimSpace(c.User) == "" || c.Password == "" {
		return nil, ErrConnectionConfig
	}
	switch c.SSLMode {
	case "disable", "require", "verify-ca", "verify-full":
	default:
		return nil, ErrConnectionConfig
	}
	u := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))), Path: "/" + c.Database, User: url.UserPassword(c.User, c.Password)}
	query := url.Values{"sslmode": {c.SSLMode}, "passfile": {os.DevNull}, "sslcert": {""}, "sslkey": {""}, "sslrootcert": {""}}
	if strings.HasPrefix(c.Host, "/") {
		u.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(c.Port)))
		query.Set("host", c.Host)
	}
	u.RawQuery = query.Encode()
	return ParsePoolConfig(u.String())
}
func QuoteLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func ParseConnection(dsn string) (Connection, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return Connection{}, ErrConnectionConfig
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil {
		return Connection{}, ErrConnectionConfig
	}
	password, _ := u.User.Password()
	host := u.Hostname()
	if socket := u.Query().Get("host"); socket != "" {
		host = socket
	}
	mode := u.Query().Get("sslmode")
	if mode == "" {
		mode = "disable"
	}
	return Connection{Host: host, Port: uint16(port), User: u.User.Username(), Password: password, Database: strings.TrimPrefix(u.Path, "/"), SSLMode: mode}, nil
}

func ParsePoolConfig(dsn string) (*pgxpool.Config, error) {
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") && value != "" {
			return nil, ErrConnectionConfig
		}
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Fragment != "" || u.Hostname() == "" || strings.Contains(u.Hostname(), ",") || u.User.Username() == "" || strings.TrimPrefix(u.Path, "/") == "" {
		return nil, ErrConnectionConfig
	}
	password, present := u.User.Password()
	if !present || password == "" {
		return nil, ErrConnectionConfig
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || port == 0 {
		return nil, ErrConnectionConfig
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, ErrConnectionConfig
	}
	allowed := map[string]bool{"host": true, "sslmode": true, "sslnegotiation": true, "sslrootcert": true, "sslcert": true, "sslkey": true, "sslpassword": true, "connect_timeout": true, "channel_binding": true, "application_name": true, "target_session_attrs": true, "min_protocol_version": true, "max_protocol_version": true, "default_query_exec_mode": true, "statement_cache_capacity": true, "description_cache_capacity": true, "passfile": true}
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			return nil, ErrConnectionConfig
		}
	}
	if file := query.Get("passfile"); file != "" && file != os.DevNull {
		return nil, ErrConnectionConfig
	}
	switch query.Get("sslmode") {
	case "disable", "require", "verify-ca", "verify-full":
	default:
		return nil, ErrConnectionConfig
	}
	if query.Get("sslmode") == "require" && query.Get("sslrootcert") != "" {
		return nil, ErrConnectionConfig
	}
	query.Set("passfile", os.DevNull)
	for _, key := range []string{"sslcert", "sslkey", "sslrootcert"} {
		if _, present := query[key]; !present {
			query.Set(key, "")
		}
	}
	u.RawQuery = query.Encode()
	config, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, ErrConnectionConfig
	}
	Configure(config)
	return config, nil
}
