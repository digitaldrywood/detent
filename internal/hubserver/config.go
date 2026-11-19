package hubserver

import (
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/hubconfig"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
)

const (
	DefaultListenAddress              = hubconfig.DefaultListenAddress
	DefaultWebhookPayloadRetention    = hubconfig.DefaultWebhookPayloadRetention
	DefaultWebhookMaintenanceInterval = hubconfig.DefaultWebhookMaintenanceInterval
	DefaultReconcileInterval          = hubconfig.DefaultReconcileInterval
	DefaultFullRepairInterval         = hubconfig.DefaultFullRepairInterval
)

var (
	ErrBackupSource            = errors.New("hub backup destination matches the source database")
	ErrDatabaseIdentity        = errors.New("database is not a Detent Hub database")
	ErrInvalidClock            = errors.New("hub clock returned a zero time")
	ErrInsecureListener        = errors.New("non-loopback hub listener requires TLS or a trusted reverse proxy")
	ErrNetworkFilesystem       = hubconfig.ErrNetworkFilesystem
	ErrNotReady                = errors.New("hub service is not ready")
	ErrOutboxDisabled          = errors.New("hub github outbox is disabled")
	ErrUnsupportedSchema       = errors.New("hub database schema is newer than this Detent version")
	ErrWebhookDeliveryConflict = errors.New("github webhook delivery ID has conflicting content")
)

type Config = hubconfig.Config

func WithDeps(cfg Config, deps hubdeps.Deps) Config {
	cfg.Deps = deps
	return cfg
}
