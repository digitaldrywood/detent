package billing

import (
	"errors"
	"strconv"
	"strings"
)

type CreditAdjustment struct {
	IdempotencyKey string `json:"idempotency_key"`
	AmountUSD      string `json:"amount_usd"`
	Reason         string `json:"reason"`
}

func (a CreditAdjustment) AmountMicros() (int64, error) {
	amount := a.AmountUSD
	negative := strings.HasPrefix(amount, "-")
	if negative {
		amount = strings.TrimPrefix(amount, "-")
	}
	whole, fraction, _ := strings.Cut(amount, ".")
	if whole == "" || len(fraction) > 6 || strings.HasSuffix(amount, ".") {
		return 0, errors.New("USD amount must be a nonzero decimal with at most six decimal places")
	}
	digits := whole + fraction
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, errors.New("USD amount must be a nonzero decimal with at most six decimal places")
		}
	}
	digits += strings.Repeat("0", 6-len(fraction))
	if negative {
		digits = "-" + digits
	}
	micros, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || micros == 0 {
		return 0, errors.New("USD amount is zero or out of range")
	}
	return micros, nil
}
