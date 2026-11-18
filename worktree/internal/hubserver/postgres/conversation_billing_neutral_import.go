package postgres

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func readNeutralConversationBillingPage(ctx context.Context, source ConversationBillingImportSource, binding *storagecatalog.Binding, table string, columns, keys []string, after []any) ([]map[string]storagecatalog.Value, []any, error) {
	var cursor []string
	for _, value := range after {
		cursor = append(cursor, fmt.Sprint(value))
	}
	rows, err := source.neutral.Page(ctx, table, cursor, 100)
	if err != nil {
		return nil, nil, err
	}
	result := make([]map[string]storagecatalog.Value, 0, len(rows))
	var next []any
	for _, row := range rows {
		values := make(map[string]storagecatalog.Value, len(columns))
		for _, name := range columns {
			value, err := source.Catalog.Decode("hub/"+table, name, row.Values[name])
			if err != nil {
				return nil, nil, err
			}
			values[name] = value
		}
		if _, err := binding.ValidateMappedRow(table, row.Values); err != nil {
			return nil, nil, err
		}
		next = make([]any, len(row.Key))
		for index, value := range row.Key {
			next[index] = value
		}
		result = append(result, values)
	}
	return result, next, nil
}
