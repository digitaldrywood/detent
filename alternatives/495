package billing

import "testing"

func TestCreditAdjustmentAmount(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		amount  string
		want    int64
		invalid bool
	}{
		{amount: "10", want: 10000000},
		{amount: "-2.50", want: -2500000},
		{amount: "0.000001", want: 1},
		{amount: "9223372036854.775807", want: 9223372036854775807},
		{amount: "-9223372036854.775808", want: -9223372036854775808},
		{amount: "", invalid: true},
		{amount: "0", invalid: true},
		{amount: "-0.00", invalid: true},
		{amount: ".25", invalid: true},
		{amount: "1.", invalid: true},
		{amount: "1.2.3", invalid: true},
		{amount: " 1", invalid: true},
		{amount: "1e3", invalid: true},
		{amount: "+1", invalid: true},
		{amount: "0.0000001", invalid: true},
		{amount: "9223372036854.775808", invalid: true},
	} {
		t.Run(test.amount, func(t *testing.T) {
			amount, err := (CreditAdjustment{AmountUSD: test.amount}).AmountMicros()
			if (err != nil) != test.invalid || err == nil && amount != test.want {
				t.Fatalf("micros=%d err=%v", amount, err)
			}
		})
	}
}
