//go:build !darwin && !linux

package gotestreceipt

func withSocketPath(path string, action func(string) error) error {
	return action(path)
}
