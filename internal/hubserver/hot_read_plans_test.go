package hubserver

type tracedStatement struct {
	query string
	args  []any
}
