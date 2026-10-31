package cloudentry

import (
	"fmt"
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	for _, key := range []string{"RESEND_API_KEY", "DETENT_HUB_SECRET_KEYS", "DETENT_HUB_SECRET_KEY_VERSION"} {
		os.Unsetenv(key)
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
