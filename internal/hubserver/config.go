package hubserver

import (
	"errors"
	"log/slog"
	"time"
)

const (
	DefaultListenAddress              = "127.0.0.1:7777"
	DefaultWebhookPayloadRetention    = 7 * 24 * time.Hour
	DefaultWebhookMaintenanceInterval = time.Minute
	defaultBusyTimeout                = 5 * time.Second
	defaultShutdownTime               = 5 * time.Second
)

var (
	ErrBackupSource            = errors.New("hub backup destination matches the source database")
	ErrDatabaseIdentity        = errors.New("database is not a Detent Hub database")
	ErrNetworkFilesystem       = errors.New("hub database requires a local filesystem")
	ErrNotReady                = errors.New("hub service is not ready")
	ErrUnsupportedSchema       = errors.New("hub database schema is newer than this Detent version")
	ErrWebhookDeliveryConflict = errors.New("github webhook delivery ID has conflicting content")
)

type Config struct {
	DatabasePath               string
	ListenAddress              string
	BusyTimeout                time.Duration
	ShutdownTimeout            time.Duration
	GitHubWebhookSecret        []byte
	WebhookPayloadRetention    time.Duration
	WebhookMaintenanceInterval time.Duration
	Logger                     *slog.Logger
	Version                    string

	now                        func() time.Time
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
	if c.WebhookPayloadRetention <= 0 {
		c.WebhookPayloadRetention = DefaultWebhookPayloadRetention
	}
	if c.WebhookMaintenanceInterval <= 0 {
		c.WebhookMaintenanceInterval = DefaultWebhookMaintenanceInterval
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	c.GitHubWebhookSecret = append([]byte(nil), c.GitHubWebhookSecret...)
	if c.validateDatabaseFilesystem == nil {
		c.validateDatabaseFilesystem = validateLocalDatabaseFilesystem
	}
	return c
}
