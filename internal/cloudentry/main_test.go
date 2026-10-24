package cloudentry

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	os.Unsetenv("RESEND_API_KEY")
	os.Exit(m.Run())
}
