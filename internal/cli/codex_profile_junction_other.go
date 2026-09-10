//go:build !windows

package cli

import "errors"

func createCodexProfileJunction(string, string) error {
	return errors.New("codex directory junctions require Windows")
}
