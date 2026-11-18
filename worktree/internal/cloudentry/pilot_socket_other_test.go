//go:build !darwin && !windows

package cloudentry

func pilotSocketPath(path string, action func(string) error) error {
	return action(path)
}
