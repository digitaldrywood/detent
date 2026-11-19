package hubserver

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/changefiles/sourcetest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func changeTestSource(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) []byte {
	t.Helper()
	return sourcetest.Source(t, input, content...)
}

func changeTestSourceCheckout(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) ([]byte, string) {
	t.Helper()
	return sourcetest.Checkout(t, input, content...)
}

func changeTestPack(t *testing.T, kind byte, size uint64, content []byte, count uint32) []byte {
	t.Helper()
	return sourcetest.Pack(t, kind, size, content, count)
}

func validationRepository(repository string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(repository, "https://github.com/"), "git@github.com:"), ".git")
}
