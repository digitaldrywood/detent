package accessstore

import (
	"time"

	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type OperatorSession struct {
	OrganizationID                                   tracker.OrganizationID
	ID, PrincipalID, CredentialID, SessionID, Client string
	Origin                                           entryauthority.Origin
	LastUsedAt                                       time.Time
}
