package usagecost

import (
	"errors"
	"math/big"
	"time"
)

var ErrInvalidPeriod = errors.New("cost period must be positive and at most 366 days")

type MonthlyAmount struct {
	Month  time.Time
	Micros int64
}

func AllocateMonths(amount int64, from, to time.Time) ([]MonthlyAmount, error) {
	if from.IsZero() || !to.After(from) || to.Sub(from) > 366*24*time.Hour {
		return nil, ErrInvalidPeriod
	}
	from, to = from.UTC(), to.UTC()
	month := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
	duration := big.NewInt(to.Sub(from).Nanoseconds())
	boundary := func(at time.Time) *big.Int {
		product := new(big.Int).Mul(big.NewInt(amount), big.NewInt(at.Sub(from).Nanoseconds()))
		return product.Quo(product, duration)
	}
	var result []MonthlyAmount
	for month.Before(to) {
		next := month.AddDate(0, 1, 0)
		start, end := month, next
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		micros := new(big.Int).Sub(boundary(end), boundary(start)).Int64()
		result = append(result, MonthlyAmount{Month: month, Micros: micros})
		month = next
	}
	return result, nil
}
