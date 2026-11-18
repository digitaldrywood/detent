package credits

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type Balance struct {
	BalanceMicros int64         `json:"balance_micros"`
	SpentMicros   int64         `json:"spent_micros"`
	Month         string        `json:"month"`
	AutoEnabled   bool          `json:"auto_enabled"`
	History       []Transaction `json:"history"`
}

func (s *Service) PlatformBalance(ctx context.Context) (Balance, error) {
	var result Balance

	var creditRows []billingstore.CreditTransaction
	now := s.deps.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	result.Month = start.Format("2006-01")
	result.History = []Transaction{}

	err := s.read(ctx, func(reader tenantstore.Reader) error {
		account, err := reader.BillingCreditAccount(ctx, s.deps.Mode)
		if err != nil {
			return err
		}
		result.BalanceMicros, result.AutoEnabled = account.BalanceMicros, account.AutoEnabled
		result.SpentMicros, err = reader.BillingCreditUsage(ctx, s.deps.Mode, start, start.AddDate(0, 1, 0))
		if err != nil {
			return err
		}
		rows, err := reader.BillingRecentCreditTransactions(ctx, s.deps.Mode, 50)
		if err != nil {
			return err
		}
		creditRows = rows
		return nil
	})
	if err != nil {
		return result, err
	}
	for _, row := range creditRows {
		reason, err := s.deps.ReadReason(ctx, row.ReasonReference)
		if err != nil {
			return result, err
		}
		result.History = append(result.History, Transaction{AmountMicros: row.AmountMicros, Kind: row.Kind, Actor: row.Actor, Reason: reason, At: row.RecordedAt.UTC()})
	}
	return result, nil
}
