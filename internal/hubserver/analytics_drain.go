package hubserver

type projectionQuery interface {
	nativeQueryer
	nativeExecer
}
