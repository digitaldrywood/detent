package credits

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

type Page struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
}

type historyCursor struct {
	Snapshot int64 `json:"snapshot"`
	At       int64 `json:"at"`
	ID       int64 `json:"id"`
	From     int64 `json:"from"`
	To       int64 `json:"to"`
}

func (p Page) Bounds() (int64, int64, error) {
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
			return 0, 0, hubapi.Invalid("invalid credit transaction period")
		}
		*item.target = at.UnixMicro()
	}
	if from >= to {
		return 0, 0, hubapi.Invalid("invalid credit transaction period")
	}
	return from, to, nil
}

func (s *Service) History(ctx context.Context, view *View, p Page) error {
	from, to, err := p.Bounds()
	if err != nil {
		return err
	}
	if p.Limit == 0 {
		p.Limit = 10
	}
	if p.Limit < 1 || p.Limit > 100 {
		return hubapi.Invalid("credit transaction limit must be between 1 and 100")
	}
	cursor := historyCursor{At: math.MaxInt64, ID: math.MaxInt64, From: from, To: to}

	if p.Cursor != "" {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(p.Cursor)
		if decodeErr != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Snapshot < 0 || cursor.At < 0 || cursor.ID <= 0 || cursor.From != from || cursor.To != to {
			return hubapi.Invalid("invalid credit transaction cursor")
		}
	}
	if cursor.At == math.MaxInt64 {
		cursor.At = 253402300799999999
	}
	var rows []billingstore.CreditHistoryEntry
	err = s.read(ctx, func(reader tenantstore.Reader) error {
		var snapshot int64
		var err error
		snapshot, view.TotalRows, rows, err = reader.BillingCreditHistory(ctx, s.deps.Mode, cursor.Snapshot, from, to, cursor.At, cursor.ID, p.Limit+1)
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
		reason, err := s.deps.ReadReason(ctx, row.ReasonReference)
		if err != nil {
			return err
		}
		view.History = append(view.History, Transaction{AmountMicros: row.AmountMicros, IncludedAmountMicros: row.IncludedAmountMicros, Kind: row.Kind, Actor: row.Actor, Reason: reason, At: row.RecordedAt.UTC(), Turns: row.Turns})
		lastID, lastAt = row.ID, row.RecordedAt.UnixMicro()
	}
	return nil
}

func CSVCell(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" && strings.ContainsAny(trimmed[:1], "=+-@") || strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") {
		return "'" + value
	}
	return value
}

func WriteCSVEntry(writer *csv.Writer, row billingstore.CreditExportEntry) error {
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
	if err := writer.Write([]string{at.UTC().Format(time.RFC3339Nano), kind, CSVCell(turn), tokenText, dollars, fmt.Sprintf("%d.%06d", included/1000000, included%1000000)}); err != nil {
		return err
	}
	return nil
}
