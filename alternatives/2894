//go:build !unix

package store

import "context"

func lockMigrations(context.Context, string) (func() error, error) {
	return func() error { return nil }, nil
}
