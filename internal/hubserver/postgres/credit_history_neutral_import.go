package postgres

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func validateNeutralCreditHistory(ctx context.Context, source CreditHistoryImportSource) error {
	for _, table := range append(slices.Clone(creditHistoryImportTables), creditHistoryImportTable{name: "ai_credit_periods", keys: []string{"organization_id", "mode", "period_start", "period_end"}}, creditHistoryImportTable{name: "ai_credit_purchases", keys: []string{"purchase_key"}}) {
		if err := source.neutral.ValidateCatalogTable(source.Catalog, source.Schema, table.name, table.keys); err != nil {
			return err
		}
	}
	for _, name := range []string{"ai_credit_periods", "ai_credit_purchases"} {
		rows, err := source.neutral.OrdinalPage(ctx, name, 0, 1)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			return errUnsupportedCreditHistory
		}
	}
	binding, err := source.Catalog.Bind(source.Source, source.Schema)
	if err != nil {
		return err
	}
	for _, table := range creditHistoryImportTables {
		var after int64
		for {
			rows, err := readNeutralCreditHistoryPage(ctx, source, binding, table, after)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				break
			}
			after = rows[len(rows)-1].position
		}
	}
	return nil
}
func readNeutralCreditHistoryPage(ctx context.Context, source CreditHistoryImportSource, binding *storagecatalog.Binding, table creditHistoryImportTable, after int64) ([]creditHistorySourceRow, error) {
	rows, err := source.neutral.OrdinalPage(ctx, table.name, after, creditHistoryImportPageSize)
	if err != nil {
		return nil, err
	}
	result := make([]creditHistorySourceRow, 0, len(rows))
	definition := source.Catalog.Tables["hub/"+table.name]
	for _, raw := range rows {
		row := creditHistorySourceRow{id: raw.SourceRowID, position: raw.Ordinal, cells: raw.Values, value: map[string]storagecatalog.Value{}}
		for name, column := range definition.Columns {
			if column.Disposition == "column" {
				value, err := source.Catalog.Decode("hub/"+table.name, name, row.cells[name])
				if err != nil {
					return nil, err
				}
				row.value[name] = value
			}
		}
		if _, err := binding.ValidateMappedRow(table.name, row.cells); err != nil {
			return nil, err
		}
		if row.id < 1 {
			return nil, storagecatalog.ErrValue
		}
		result = append(result, row)
	}
	return result, nil
}
