//go:build unix && !darwin

package hookproc

import (
	"context"
	"errors"
)

func (*Lifetime) platformWriters(context.Context) ([]int, error) { return nil, errors.ErrUnsupported }
func (*Lifetime) platformHoldsWriter(int) (bool, error)          { return false, errors.ErrUnsupported }
