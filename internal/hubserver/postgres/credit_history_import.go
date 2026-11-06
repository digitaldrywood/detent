package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const creditHistoryImportPageSize = 100

var errUnsupportedCreditHistory = errors.New("credit history contains unsupported period or purchase rows")

type CreditHistoryImportSource struct {
	CopyPath string
	Catalog  storagecatalog.Catalog
	Source   storagecatalog.Source
	Schema   storagecatalog.Schema
	Objects  attachment.DetailStore
}

type CreditHistoryImportResult struct {
	Accounts     int64
	Packs        int64
	Transactions int64
}

type creditHistoryImportTable struct {
	name string
	keys []string
}

type creditHistorySourceRow struct {
	id    int64
	cells map[string]storagecatalog.Cell
	value map[string]storagecatalog.Value
}

var creditHistoryImportTables = []creditHistoryImportTable{
	{name: "ai_credit_accounts", keys: []string{"organization_id", "mode"}},
	{name: "ai_credit_packs", keys: []string{"price_id", "mode"}},
	{name: "ai_credit_transactions", keys: []string{"id"}},
}

func (r *Repository) ImportCreditHistory(ctx context.Context, source CreditHistoryImportSource) (CreditHistoryImportResult, error) {
	var result CreditHistoryImportResult
	if source.CopyPath == "" || source.Objects == nil || source.Source.Store != "hub" || strings.TrimSpace(source.Source.OrganizationID) != source.Source.OrganizationID || source.Source.OrganizationID == "" {
		return result, errors.New("invalid credit history import source")
	}
	if source.Schema == nil {
		source.Schema = schemaFromCatalog(source.Catalog)
	}
	source.Source.Schema = "hub_copy"
	if _, err := source.Catalog.Bind(source.Source, source.Schema); err != nil {
		return result, err
	}
	organization := tracker.OrganizationID(source.Source.OrganizationID)
	catalogDigest, err := conversationImportCatalogDigest(source.Catalog, source.Schema)
	if err != nil {
		return result, err
	}
	completed := false
	snapshotID := ""
	err = ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		snapshotID = copyHash
		for _, table := range append(slices.Clone(creditHistoryImportTables), creditHistoryImportTable{name: "ai_credit_periods", keys: []string{"organization_id", "mode", "period_start", "period_end"}}, creditHistoryImportTable{name: "ai_credit_purchases", keys: []string{"purchase_key"}}) {
			if err := validateConversationBillingCopyTable(ctx, tx, source.Catalog, source.Schema, table.name, table.keys); err != nil {
				return fmt.Errorf("validate %s source: %w", table.name, err)
			}
		}
		for _, table := range []string{"ai_credit_periods", "ai_credit_purchases"} {
			var count int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM "`+table+`" WHERE organization_id=?`, organization).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errUnsupportedCreditHistory
			}
		}
		if err := r.PinCoreImportSource(ctx, organization, copyHash, source.Objects); err != nil {
			return err
		}
		var err error
		completed, result, err = r.beginCreditHistoryImport(ctx, organization, source.Source.Schema, copyHash, catalogDigest)
		if err != nil || completed {
			return err
		}
		return r.importCreditHistoryCopy(ctx, tx, copyHash, source, organization, &result)
	})
	if err != nil || completed {
		return result, err
	}
	if err := r.advanceCreditTransactionSequence(ctx, organization); err != nil {
		return result, err
	}
	if err := r.completeCreditHistoryImport(ctx, organization, source.Source.Schema, snapshotID, catalogDigest, result); err != nil {
		return result, err
	}
	return result, nil
}

func (r *Repository) beginCreditHistoryImport(ctx context.Context, organization tracker.OrganizationID, sourceSchema, snapshotID, catalogDigest string) (bool, CreditHistoryImportResult, error) {
	var completed bool
	var result CreditHistoryImportResult
	err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		if _, err := s.db.Exec(ctx, `INSERT INTO credit_history_imports(organization_id,source_schema,snapshot_id,catalog_digest) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id) DO NOTHING`, organization, sourceSchema, snapshotID, catalogDigest); err != nil {
			return mapError(ctx, err)
		}
		var storedSchema, storedSnapshot, storedCatalog string
		err := s.db.QueryRow(ctx, `SELECT source_schema,snapshot_id,catalog_digest,completed,account_rows,pack_rows,transaction_rows FROM credit_history_imports WHERE organization_id=$1`, organization).Scan(&storedSchema, &storedSnapshot, &storedCatalog, &completed, &result.Accounts, &result.Packs, &result.Transactions)
		if err != nil {
			return mapError(ctx, err)
		}
		if storedSchema != sourceSchema || storedSnapshot != snapshotID || storedCatalog != catalogDigest {
			return mutation.ErrConflict
		}
		return nil
	})
	return completed, result, err
}

func (r *Repository) completeCreditHistoryImport(ctx context.Context, organization tracker.OrganizationID, sourceSchema, snapshotID, catalogDigest string, result CreditHistoryImportResult) error {
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		written, err := s.db.Exec(ctx, `UPDATE credit_history_imports SET completed=true,account_rows=$5,pack_rows=$6,transaction_rows=$7 WHERE organization_id=$1 AND source_schema=$2 AND snapshot_id=$3 AND catalog_digest=$4 AND (NOT completed OR (account_rows=$5 AND pack_rows=$6 AND transaction_rows=$7))`, organization, sourceSchema, snapshotID, catalogDigest, result.Accounts, result.Packs, result.Transactions)
		if err != nil {
			return mapError(ctx, err)
		}
		count := written.RowsAffected()
		if count != 1 {
			return mutation.ErrConflict
		}
		return nil
	})
}

func (r *Repository) importCreditHistoryCopy(ctx context.Context, tx *sql.Tx, copyHash string, source CreditHistoryImportSource, organization tracker.OrganizationID, result *CreditHistoryImportResult) error {
	binding, err := source.Catalog.Bind(source.Source, source.Schema)
	if err != nil {
		return err
	}
	for _, table := range creditHistoryImportTables {
		var after int64
		for {
			page, err := readCreditHistoryPage(ctx, tx, source, binding, table, organization, after)
			if err != nil {
				return fmt.Errorf("read %s import page: %w", table.name, err)
			}
			if len(page) == 0 {
				break
			}
			for _, row := range page {
				var reason billingstore.AuditReference
				if table.name == "ai_credit_transactions" {
					reasonCell := row.cells["reason"]
					if reasonCell.Null || reasonCell.Encoding != "utf8" {
						return storagecatalog.ErrValue
					}
					if len(reasonCell.Data) != 0 {
						reason, err = prepareCreditHistoryReason(ctx, source.Objects, organization, string(reasonCell.Data))
						if err != nil {
							return fmt.Errorf("prepare credit reason row %d: %w", row.id, err)
						}
					}
				}
				if err := r.importCreditHistoryRow(ctx, organization, copyHash, table.name, row, reason); err != nil {
					return fmt.Errorf("write %s import row %d: %w", table.name, row.id, err)
				}
				*creditHistoryCount(result, table.name)++
				after = row.id
			}
		}
	}
	return nil
}

func creditHistoryCount(result *CreditHistoryImportResult, table string) *int64 {
	switch table {
	case "ai_credit_accounts":
		return &result.Accounts
	case "ai_credit_packs":
		return &result.Packs
	default:
		return &result.Transactions
	}
}

func readCreditHistoryPage(ctx context.Context, tx *sql.Tx, source CreditHistoryImportSource, binding *storagecatalog.Binding, table creditHistoryImportTable, organization tracker.OrganizationID, after int64) ([]creditHistorySourceRow, error) {
	definition := source.Catalog.Tables["hub/"+table.name]
	columns := make([]string, 0, len(definition.Columns))
	for column := range definition.Columns {
		columns = append(columns, column)
	}
	slices.Sort(columns)
	selection := []string{"r.rowid"}
	for _, column := range columns {
		if !conversationImportIdentifier.MatchString(column) {
			return nil, storagecatalog.ErrUnmapped
		}
		selection = append(selection, `CAST(r."`+column+`" AS TEXT)`)
	}
	from := `"` + table.name + `" r`
	where := ` WHERE r.rowid>?`
	args := []any{after}
	if _, scoped := definition.Columns["organization_id"]; scoped {
		where += ` AND r.organization_id=?`
		args = append(args, organization)
	}
	var query strings.Builder
	query.WriteString("SELECT ")
	query.WriteString(strings.Join(selection, ","))
	query.WriteString(" FROM ")
	query.WriteString(from)
	query.WriteString(where)
	query.WriteString(" ORDER BY r.rowid LIMIT ?")
	args = append(args, creditHistoryImportPageSize)
	rows, err := tx.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := make([]creditHistorySourceRow, 0, creditHistoryImportPageSize)
	for rows.Next() {
		var row creditHistorySourceRow
		cells := make([]sql.NullString, len(columns))
		destinations := make([]any, 1+len(columns))
		destinations[0] = &row.id
		for i := range cells {
			destinations[i+1] = &cells[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, err
		}
		row.cells = make(map[string]storagecatalog.Cell, len(columns))
		row.value = make(map[string]storagecatalog.Value, len(columns))
		for i, name := range columns {
			cell := storagecatalog.Cell{}
			if cells[i].Valid {
				cell.Encoding = "utf8"
				cell.Data = []byte(cells[i].String)
			} else {
				cell.Null = true
			}
			row.cells[name] = cell
			column := definition.Columns[name]
			if column.Disposition == "column" {
				value, err := source.Catalog.Decode("hub/"+table.name, name, cell)
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
		page = append(page, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return page, nil
}

func (r *Repository) importCreditHistoryRow(ctx context.Context, organization tracker.OrganizationID, copyHash, table string, row creditHistorySourceRow, reason billingstore.AuditReference) error {
	encoded, err := json.Marshal(struct {
		Snapshot string
		Table    string
		Row      int64
		Cells    map[string]storagecatalog.Cell
		Reason   billingstore.AuditReference
	}{copyHash, table, row.id, row.cells, reason})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	requestHash := hex.EncodeToString(digest[:])
	createdAt := time.Unix(0, 0).UTC()
	if value, ok := row.value["recorded_at"]; ok {
		createdAt = time.UnixMicro(value.Integer).UTC()
	}
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		var previousSnapshot, previousHash string
		err := s.db.QueryRow(ctx, `SELECT snapshot_id,request_hash FROM credit_history_import_rows WHERE organization_id=$1 AND source_table=$2 AND source_row_id=$3`, organization, table, row.id).Scan(&previousSnapshot, &previousHash)
		if err == nil {
			if previousSnapshot != copyHash || previousHash != requestHash {
				return mutation.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return mapError(ctx, err)
		}
		switch table {
		case "ai_credit_accounts":
			account := billingstore.CreditAccount{OrganizationID: organization, Mode: row.value["mode"].Text, BalanceMicros: row.value["balance_micros"].Integer, AutoEnabled: row.value["auto_enabled"].Boolean, ThresholdCents: row.value["threshold_cents"].Integer, PriceID: row.value["price_id"].Text, PaymentMethod: row.value["payment_method"].Text, Failure: row.value["failure"].Text}
			if err := s.CreateBillingCreditAccount(ctx, account); err != nil {
				return err
			}
		case "ai_credit_packs":
			pack := billingstore.CreditPack{OrganizationID: organization, PriceID: row.value["price_id"].Text, Mode: row.value["mode"].Text, USDCents: row.value["usd_cents"].Integer}
			if err := s.CreateCreditPack(ctx, pack); err != nil {
				return err
			}
		case "ai_credit_transactions":
			id := row.value["id"].Integer
			micros, err := strconv.ParseInt(string(row.cells["recorded_at"].Data), 10, 64)
			if row.cells["recorded_at"].Null || err != nil {
				return storagecatalog.ErrValue
			}
			recordedAt := time.UnixMicro(micros).UTC()
			if recordedAt.Year() < 1 || recordedAt.Year() > 9999 || id != row.id {
				return storagecatalog.ErrValue
			}
			if err := validateCreditKind(row.value["kind"].Text); err != nil {
				return err
			}
			if _, err := s.db.Exec(ctx, `INSERT INTO ai_credit_transactions(organization_id,id,mode,source,amount_micros,included_amount_micros,kind,actor,recorded_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, organization, id, row.value["mode"].Text, row.value["source"].Text, row.value["amount_micros"].Integer, row.value["included_amount_micros"].Integer, row.value["kind"].Text, row.value["actor"].Text, recordedAt); err != nil {
				return mapError(ctx, err)
			}
			if reason.Key != "" {
				if reason.ArchiveReference().Validate(string(organization)) != nil {
					return billingstore.ErrInvalidAmount
				}
				if _, err := s.db.Exec(ctx, `INSERT INTO ai_credit_reason_references(organization_id,mode,source,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, organization, row.value["mode"].Text, row.value["source"].Text, reason.BindingID, reason.CustomerOwned, reason.Key, reason.SHA256, reason.CompressedBytes, reason.ContentBytes, reason.Codec, reason.FormatVersion, reason.Kind, reason.Retention); err != nil {
					return mapError(ctx, err)
				}
			}
		default:
			return storagecatalog.ErrUnmapped
		}
		_, err = s.db.Exec(ctx, `INSERT INTO credit_history_import_rows(organization_id,snapshot_id,source_table,source_row_id,request_hash,created_at) VALUES($1,$2,$3,$4,$5,$6)`, organization, copyHash, table, row.id, requestHash, createdAt)
		return mapError(ctx, err)
	})
}

func validateCreditKind(kind string) error {
	if !slices.Contains([]string{"purchase", "auto_fund", "usage", "complimentary"}, kind) {
		return storagecatalog.ErrValue
	}
	return nil
}

func prepareCreditHistoryReason(ctx context.Context, objects attachment.DetailStore, organization tracker.OrganizationID, reason string) (billingstore.AuditReference, error) {
	if !utf8.ValidString(reason) || strings.IndexByte(reason, 0) >= 0 || len(reason) > attachment.MaxArchiveBytes {
		return billingstore.AuditReference{}, storagecatalog.ErrValue
	}
	raw, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil || len(raw) > attachment.MaxArchiveBytes {
		return billingstore.AuditReference{}, errors.Join(storagecatalog.ErrValue, err)
	}
	reference, err := objects.StoreDetail(ctx, string(organization), raw)
	if err != nil || reference.Validate(string(organization)) != nil || reference.BindingID != "" || reference.CustomerOwned || reference.SchemaVersion != 1 {
		return billingstore.AuditReference{}, errors.Join(attachment.ErrInvalid, err)
	}
	stored, err := objects.ReadDetail(ctx, string(organization), reference)
	if err != nil || !bytes.Equal(stored, raw) {
		return billingstore.AuditReference{}, errors.Join(attachment.ErrInvalid, err)
	}
	return billingstore.AuditReferenceFromArchive(reference), nil
}

func (r *Repository) advanceCreditTransactionSequence(ctx context.Context, organization tracker.OrganizationID) error {
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		_, err := s.db.Exec(ctx, `SELECT advance_imported_credit_transaction_id(COALESCE((SELECT max(id) FROM ai_credit_transactions),0))`)
		return mapError(ctx, err)
	})
}
