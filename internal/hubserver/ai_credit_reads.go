package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

type creditPageRequest struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
}

type creditHistoryCursor struct {
	Snapshot int64 `json:"snapshot"`
	At       int64 `json:"at"`
	ID       int64 `json:"id"`
	From     int64 `json:"from"`
	To       int64 `json:"to"`
}

func (p creditPageRequest) bounds() (int64, int64, error) {
	from, to := int64(0), int64(math.MaxInt64)
	for _, item := range []struct {
		value  string
		target *int64
	}{{p.From, &from}, {p.To, &to}} {
		if item.value == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, item.value)
		if err != nil || at.UnixMicro() < 0 {
			return 0, 0, nativeInvalid("invalid credit transaction period")
		}
		*item.target = at.UnixMicro()
	}
	if from >= to {
		return 0, 0, nativeInvalid("invalid credit transaction period")
	}
	return from, to, nil
}

func creditPageQuery(c echo.Context) (creditPageRequest, error) {
	p := creditPageRequest{Cursor: c.QueryParam("cursor"), From: c.QueryParam("from"), To: c.QueryParam("to")}
	if value := c.QueryParam("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			return p, nativeInvalid("credit transaction limit must be between 1 and 100")
		}
		p.Limit = n
	}
	return p, nil
}

const groupedCreditHistory = `WITH grouped AS (
 SELECT max(id) AS row_id, sum(amount_micros) AS amount, sum(included_amount_micros) AS included, kind, max(actor) AS actor, max(reason) AS reason,
 max(recorded_at) AS at, CASE WHEN kind='usage' THEN count(*) ELSE 0 END AS turns
 FROM ai_credit_transactions
 WHERE organization_id=? AND mode=? AND id<=? AND recorded_at>=? AND recorded_at<?
 GROUP BY CASE WHEN kind='usage' THEN 'day:' || strftime('%Y-%m-%d',recorded_at/1000000,'unixepoch') ELSE 'id:' || id END, kind
) `

func (s *Service) readCreditHistory(ctx context.Context, view *aiCreditView, p creditPageRequest) error {
	from, to, err := p.bounds()
	if err != nil {
		return err
	}
	if p.Limit == 0 {
		p.Limit = 10
	}
	if p.Limit < 1 || p.Limit > 100 {
		return nativeInvalid("credit transaction limit must be between 1 and 100")
	}
	cursor := creditHistoryCursor{At: math.MaxInt64, ID: math.MaxInt64, From: from, To: to}
	d := s.database

	if p.Cursor != "" {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(p.Cursor)
		if decodeErr != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Snapshot < 0 || cursor.At < 0 || cursor.ID <= 0 || cursor.From != from || cursor.To != to {
			return nativeInvalid("invalid credit transaction cursor")
		}
	}
	if cursor.At == math.MaxInt64 {
		cursor.At = 253402300799999999
	}
	var rows []billingstore.CreditHistoryEntry
	err = d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var snapshot int64
		var err error
		snapshot, view.TotalRows, rows, err = reader.BillingCreditHistory(ctx, d.aiCreditMode, cursor.Snapshot, from, to, cursor.At, cursor.ID, p.Limit+1)
		cursor.Snapshot = snapshot
		return err
	})
	if err != nil {
		return err
	}
	var lastID, lastAt int64
	for _, row := range rows {
		if len(view.History) == p.Limit {
			cursor.At, cursor.ID = lastAt, lastID
			raw, err := json.Marshal(cursor)
			if err != nil {
				return err
			}
			view.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		reason, err := d.readBillingReason(ctx, row.ReasonReference)
		if err != nil {
			return err
		}
		view.History = append(view.History, aiCreditTransaction{AmountMicros: row.AmountMicros, IncludedAmountMicros: row.IncludedAmountMicros, Kind: row.Kind, Actor: row.Actor, Reason: reason, At: row.RecordedAt.UTC(), Turns: row.Turns})
		lastID, lastAt = row.ID, row.RecordedAt.UnixMicro()
	}
	return nil
}

func (s *Service) hostedCreditTransactions(c echo.Context) error {
	credential, err := s.hostedBillingOwner(c.Request().Context(), c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	p, err := creditPageQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if c.QueryParam("format") == "csv" {
		return s.exportCreditTransactions(ctx, c, credential, p)
	}
	view, err := s.readAICredits(ctx, p)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

func (s *Service) exportCreditTransactions(ctx context.Context, c echo.Context, credential apiCredential, p creditPageRequest) error {
	from, to, err := p.bounds()
	if err != nil {
		return s.nativeAPIError(c, err)
	}

	return s.exportPostgresCreditTransactions(ctx, c, credential, from, to)
}

func creditCSVCell(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" && strings.ContainsAny(trimmed[:1], "=+-@") || strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") {
		return "'" + value
	}
	return value
}

func (s *Service) creditCSVWriter(ctx context.Context, c echo.Context, credential apiCredential) (*csv.Writer, error) {
	auditCtx := mutation.WithContext(ctx, mutation.Metadata{Action: "billing_export", Source: "http", CorrelationID: s.config.newLeaseID()})
	if err := s.hostedAudit(auditCtx, credential.Hosted, "billing_exported", c.Path(), "", http.StatusOK); err != nil {
		return nil, s.nativeAPIError(c, err)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/csv; charset=utf-8")
	c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="credit-transactions.csv"`)
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	writer := csv.NewWriter(c.Response())
	if err := writer.Write([]string{"recorded_at", "kind", "turn_id", "tokens", "charged_amount_usd", "included_amount_usd"}); err != nil {
		return nil, err
	}
	return writer, nil
}

func writeCreditCSVEntry(writer *csv.Writer, row billingstore.CreditExportEntry) error {
	at, kind, turn, tokens, amount, included := row.RecordedAt, row.Kind, row.TurnID, row.Tokens, row.AmountMicros, row.IncludedAmountMicros
	tokenText := ""
	if tokens != nil {
		tokenText = strconv.FormatInt(*tokens, 10)
	}
	signed := strconv.FormatInt(amount, 10)
	sign := ""
	if strings.HasPrefix(signed, "-") {
		sign, signed = "-", signed[1:]
	}
	signed = strings.Repeat("0", max(0, 7-len(signed))) + signed
	dollars := sign + signed[:len(signed)-6] + "." + signed[len(signed)-6:]
	if err := writer.Write([]string{at.UTC().Format(time.RFC3339Nano), kind, creditCSVCell(turn), tokenText, dollars, fmt.Sprintf("%d.%06d", included/1000000, included%1000000)}); err != nil {
		return err
	}
	return nil
}

func (s *Service) exportPostgresCreditTransactions(ctx context.Context, c echo.Context, credential apiCredential, from, to int64) error {
	const pageSize = 100
	var snapshot int64
	beforeAt, beforeID := time.UnixMicro(math.MaxInt64), int64(math.MaxInt64)
	var writer *csv.Writer
	for {
		var rows []billingstore.CreditExportEntry
		err := s.database.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
			var err error
			snapshot, rows, err = reader.BillingCreditExport(ctx, s.database.aiCreditMode, snapshot, from, to, beforeAt, beforeID, pageSize)
			return err
		})
		if err != nil {
			if writer == nil {
				return s.nativeAPIError(c, err)
			}
			return err
		}
		if writer == nil {
			writer, err = s.creditCSVWriter(ctx, c, credential)
			if err != nil {
				return err
			}
		}
		for _, row := range rows {
			if err := writeCreditCSVEntry(writer, row); err != nil {
				return err
			}
			beforeAt, beforeID = row.RecordedAt, row.ID
		}
		if len(rows) < pageSize {
			break
		}
	}
	writer.Flush()
	return writer.Error()
}
