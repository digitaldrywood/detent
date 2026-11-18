//go:build unix && !darwin

package hookproc

import "os"

func platformHookSignals(*Lifetime) hookSignals { return nil }

func exactAttemptScratch(string) bool                    { return false }
func attemptScratchFD() string                           { return "" }
func holdsAttemptScratch(int, os.FileInfo) (bool, error) { return false, nil }
