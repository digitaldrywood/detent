package hubserver

import "math"

type projectionQuery interface {
	nativeQueryer
	nativeExecer
}

func projectionUpper(before []int64) int64 {
	if len(before) > 0 {
		return before[0]
	}
	return math.MaxInt64
}
