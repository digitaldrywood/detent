package workspace

import "context"

var openSessions pathLeaseRegistry

func HoldSession(path string) func() {
	release, _ := openSessions.Use(context.Background(), path)
	return release
}

func sessionHeld(path string) bool {
	return openSessions.Held(path)
}
