package postgres

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/usagecost"
)

func TestCostImportRejectsAttributionAndPayloadDrift(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC)
	amount := int64(73)
	observation := usagecost.Observation{Provider: "example", ProviderAccount: "account", ResourceID: "resource", Bucket: "runner_api", Metric: "tokens", SourceID: "source", From: from, To: from.Add(time.Nanosecond), Revision: 3, AmountMicros: &amount, Voided: true}
	payload, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	base := costImportRecord{OrganizationID: "org_example", ProjectID: "project_example", Provider: observation.Provider, ProviderAccount: observation.ProviderAccount, ResourceID: observation.ResourceID, Bucket: observation.Bucket, Metric: observation.Metric, SourceID: observation.SourceID, From: observation.From, To: observation.To, Revision: 3, ReceivedAt: from.Add(time.Hour), Payload: payload}
	for _, test := range []struct {
		name   string
		mutate func(*costImportRecord)
		valid  bool
	}{
		{"legacy nullable void uses payload", func(*costImportRecord) {}, true},
		{"typed void agrees", func(r *costImportRecord) { r.Voided = new(true) }, true},
		{"cross tenant", func(r *costImportRecord) { r.OrganizationID = "org_other" }, false},
		{"source mismatch", func(r *costImportRecord) { r.SourceID = "other" }, false},
		{"resource mismatch", func(r *costImportRecord) { r.ResourceID = "other" }, false},
		{"revision mismatch", func(r *costImportRecord) { r.Revision++ }, false},
		{"submicrosecond period mismatch", func(r *costImportRecord) { r.To = r.To.Add(time.Nanosecond) }, false},
		{"typed void disagrees", func(r *costImportRecord) { r.Voided = new(false) }, false},
		{"unknown payload field", func(r *costImportRecord) {
			r.Payload = append(append([]byte{}, payload[:len(payload)-1]...), []byte(",\"future_amount\":9}")...)
		}, false},
		{"trailing JSON", func(r *costImportRecord) { r.Payload = append(append([]byte{}, payload...), []byte(" {}")...) }, false},
		{"missing receipt time", func(r *costImportRecord) { r.ReceivedAt = time.Time{} }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := base
			test.mutate(&row)
			got, err := decodeCostImportRecord("org_example", row)
			if !test.valid {
				if !errors.Is(err, ErrCostImport) {
					t.Fatalf("import error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.ProjectID != "project_example" || got.Observation.AmountMicros == nil || *got.Observation.AmountMicros != 73 || !got.Observation.Voided || !got.Observation.From.Equal(from) || !got.Observation.To.Equal(from.Add(time.Nanosecond)) {
				t.Fatalf("import lost source facts: %+v", got)
			}
		})
	}
}
