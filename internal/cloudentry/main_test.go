package cloudentry

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	for _, key := range []string{"RESEND_API_KEY", "DETENT_HUB_SECRET_KEYS", "DETENT_HUB_SECRET_KEY_VERSION"} {
		os.Unsetenv(key)
	}
	os.Exit(m.Run())
}
