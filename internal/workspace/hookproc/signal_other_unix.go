//go:build unix && !darwin

package hookproc

func platformHookSignals(*Lifetime) hookSignals { return nil }
