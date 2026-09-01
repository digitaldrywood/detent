package hubserver

import (
	"errors"
	"log/slog"
	"time"
)

const (
	DefaultListenAddress = "127.0.0.1:7777"
	defaultBusyTimeout   = 5 * time.Second
	defaultShutdownTime  = 5 * time.Second
)

var (
	ErrBackupSource      = errors.New("hub backup destination matches the source database")
	ErrDatabaseIdentity  = errors.New("database is not a Detent Hub database")
	ErrNetworkFilesystem = errors.New("hub database requires a local filesystem")
	ErrNotReady          = errors.New("hub service is not ready")
	ErrUnsupportedSchema = errors.New("hub database schema is newer than this Detent version")
)

type Config struct {
	DatabasePath    string
	ListenAddress   string
	BusyTimeout     time.Duration
	ShutdownTimeout time.Duration
	Logger          *slog.Logger
	Version         string

	validateDatabaseFilesystem func(string) error
}

func (c Config) normalized() Config {
	if c.ListenAddress == "" {
		c.ListenAddress = DefaultListenAddress
	}
	if c.BusyTimeout <= 0 {
		c.BusyTimeout = defaultBusyTimeout
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = defaultShutdownTime
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.validateDatabaseFilesystem == nil {
		c.validateDatabaseFilesystem = validateLocalDatabaseFilesystem
	}
	return c
}
