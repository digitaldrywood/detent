package pgcore

import (
	"context"
	"time"
)

type transactionTimingKey struct{}

type transactionTiming struct {
	span      func(string, time.Time)
	statement func(time.Time, string)
}

func WithTransactionTiming(ctx context.Context, observe func(string, time.Time), statements ...func(time.Time, string)) context.Context {
	timing := transactionTiming{span: observe}
	if len(statements) > 0 {
		timing.statement = statements[0]
	}
	return context.WithValue(ctx, transactionTimingKey{}, timing)
}

func TransactionSpan(ctx context.Context, name string) func() {
	timing, ok := ctx.Value(transactionTimingKey{}).(transactionTiming)
	observe := timing.span
	if !ok || observe == nil {
		return func() {}
	}
	started := time.Now()
	return func() { observe(name, started) }
}
