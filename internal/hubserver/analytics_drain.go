package hubserver

import "github.com/digitaldrywood/detent/internal/hubserver/hubapi"

type projectionQuery interface {
	hubapi.NativeQueryer
	nativeExecer
}
