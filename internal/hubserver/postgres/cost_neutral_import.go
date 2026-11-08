package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNeutralCostImportPage(ctx context.Context, source *VerifiedNeutralImport, organization string, catalog storagecatalog.Catalog, binding *storagecatalog.Binding, after [7]string, hasAfter bool, limit int) ([]costImportPageRow, error) {
	if source == nil || source.Identity().OrganizationID != tracker.OrganizationID(organization) || binding == nil || limit < 1 || limit > costImportPageLimit {
		return nil, ErrCostImport
	}
	definition, ok := catalog.Tables["hub/usage_cost_observations"]
	if !ok || len(definition.Columns) != len(costImportColumns) {
		return nil, storagecatalog.ErrUnmapped
	}
	for _, name := range costImportColumns {
		column, ok := definition.Columns[name]
		if !ok {
			return nil, storagecatalog.ErrUnmapped
		}
		if column.Disposition == "pending" {
			return nil, storagecatalog.ErrPending
		}
		if column.Disposition != "column" && column.Disposition != "object" && column.Disposition != "transform" {
			return nil, ErrCostImport
		}
	}
	var key []string
	if hasAfter {
		key = after[:]
	}
	rows, err := source.Page(ctx, "usage_cost_observations", key, limit)
	if err != nil {
		return nil, err
	}
	result := make([]costImportPageRow, 0, len(rows))
	for _, raw := range rows {
		row, err := decodeCostImportPageRow(binding, raw.Values)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, nil
}
func decodeCostImportPageRow(binding *storagecatalog.Binding, raw map[string]storagecatalog.Cell) (costImportPageRow, error) {
	var row costImportPageRow
	organization, err := binding.ValidateMappedRow("usage_cost_observations", raw)
	if err != nil {
		return row, err
	}
	for index := range row.key {
		cell := raw[costImportColumns[index]]
		if cell.Null || cell.Encoding != "utf8" {
			return row, ErrCostImport
		}
		row.key[index] = string(cell.Data)
	}
	r := &row.record
	r.OrganizationID = tracker.OrganizationID(organization)
	if err := (NeutralImportRow{Values: raw}).Decode(map[string]any{"project_id": &r.ProjectID, "provider": &r.Provider, "provider_account": &r.ProviderAccount, "source_id": &r.SourceID, "resource_id": &r.ResourceID, "bucket": &r.Bucket, "metric": &r.Metric, "revision": &r.Revision, "voided": &r.Voided}); err != nil {
		return row, err
	}
	for name, target := range map[string]*time.Time{"period_start": &r.From, "period_end": &r.To, "received_at": &r.ReceivedAt} {
		cell := raw[name]
		if cell.Null || cell.Encoding != "utf8" {
			return row, ErrCostImport
		}
		*target, err = time.Parse(time.RFC3339Nano, string(cell.Data))
		if err != nil {
			return row, ErrCostImport
		}
	}
	payload := raw["observation_json"]
	if payload.Null || payload.Encoding != "utf8" {
		return row, ErrCostImport
	}
	r.Payload = payload.Data
	if _, err := decodeCostImportRecord(tracker.OrganizationID(organization), *r); err != nil {
		return row, err
	}
	return row, nil
}
