package progresscheck

import "context"

type TextBackend interface {
	Generate(context.Context, string, string) (string, error)
}

type AI struct {
	Backend TextBackend
}
