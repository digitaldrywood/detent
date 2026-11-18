package jitter

import (
	"crypto/rand"
	"math/big"
	"time"
)

func Offset(window time.Duration) time.Duration {
	value, err := rand.Int(rand.Reader, big.NewInt(int64(window)))
	if err != nil {
		return window / 2
	}
	return time.Duration(value.Int64())
}
