package healthprojection

import (
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type Service struct {
	Postgres           *postgres.Repository
	Tenants            tenantstore.Repository
	Details            attachment.DetailStore
	Clock              func() time.Time
	Logger             *slog.Logger
	SlackWebhookSecret string
}
