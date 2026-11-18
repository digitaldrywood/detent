package postgres

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

var ErrCostImport = errors.New("invalid tenant cost import")

type costImportRecord struct {
	OrganizationID  tracker.OrganizationID
	ProjectID       tracker.ProjectID
	Provider        string
	ProviderAccount string
	ResourceID      string
	Bucket          string
	Metric          string
	SourceID        string
	From            time.Time
	To              time.Time
	Revision        int64
	ReceivedAt      time.Time
	Voided          *bool
	Payload         []byte
}

func decodeCostImportRecord(organization tracker.OrganizationID, row costImportRecord) (billingstore.CostObservation, error) {
	if organization == "" || row.OrganizationID != organization || row.ReceivedAt.IsZero() || row.Revision < 1 || len(row.Payload) == 0 || len(row.Payload) > 1<<20 {
		return billingstore.CostObservation{}, ErrCostImport
	}
	var observation usagecost.Observation
	decoder := json.NewDecoder(bytes.NewReader(row.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observation); err != nil {
		return billingstore.CostObservation{}, ErrCostImport
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return billingstore.CostObservation{}, ErrCostImport
	}
	if observation.Provider != row.Provider || observation.ProviderAccount != row.ProviderAccount || observation.ResourceID != row.ResourceID || observation.Bucket != row.Bucket || observation.Metric != row.Metric || observation.SourceID != row.SourceID || observation.Revision != row.Revision || !observation.From.Equal(row.From) || !observation.To.Equal(row.To) {
		return billingstore.CostObservation{}, ErrCostImport
	}
	if row.Voided != nil && *row.Voided != observation.Voided {
		return billingstore.CostObservation{}, ErrCostImport
	}
	if _, err := usagecost.AllocateMonths(0, row.From, row.To); err != nil {
		return billingstore.CostObservation{}, ErrCostImport
	}
	return billingstore.CostObservation{OrganizationID: organization, ProjectID: row.ProjectID, Observation: observation, ReceivedAt: row.ReceivedAt}, nil
}
