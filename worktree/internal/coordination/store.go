package coordination

import (
	"context"
	"time"
)

type Record struct {
	Value      []byte
	Version    string
	ModifiedAt time.Time
}

type Store interface {
	Get(context.Context, string) (Record, bool, error)
	CompareAndSwap(context.Context, string, string, []byte) (Record, bool, error)
}
