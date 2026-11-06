package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func validateImportCopyPrimaryIndex(ctx context.Context, tx *sql.Tx, table string, keys []string) error {
	rows, err := tx.QueryContext(ctx, `SELECT name,origin,partial FROM pragma_index_list(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	var primary string
	for rows.Next() {
		var name, origin string
		var partial int
		if err := rows.Scan(&name, &origin, &partial); err != nil {
			return err
		}
		if origin == "pk" {
			if partial != 0 || primary != "" {
				return storagecatalog.ErrUnmapped
			}
			primary = name
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if primary == "" {
		if (table == "conversation_prices" || table == "ai_credit_transactions") && slices.Equal(keys, []string{"id"}) {
			return nil
		}
		return storagecatalog.ErrUnmapped
	}
	rows, err = tx.QueryContext(ctx, `SELECT name,"desc",coll,"key" FROM pragma_index_xinfo(?) WHERE "key"=1 ORDER BY seqno`, primary)
	if err != nil {
		return err
	}
	defer rows.Close()
	var actual []string
	for rows.Next() {
		var name, collation string
		var desc, key int
		if err := rows.Scan(&name, &desc, &collation, &key); err != nil {
			return err
		}
		if desc != 0 || collation != "BINARY" || key != 1 {
			return storagecatalog.ErrUnmapped
		}
		actual = append(actual, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if !slices.Equal(actual, keys) {
		return storagecatalog.ErrUnmapped
	}
	return nil
}
