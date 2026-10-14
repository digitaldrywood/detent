//go:build !unix

package workspace

import "testing"

func useRealProcessScan(*testing.T, string) {}

func stubReapProcessScanner() {}
