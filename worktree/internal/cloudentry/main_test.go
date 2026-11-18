package cloudentry

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

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
	flag.Parse()
	deadline, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil || deadline <= 0 {
		fmt.Fprintln(os.Stderr, "entry tests require a process deadline")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	code := m.Run()
	if err := entryPostgresTemplate.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	cancel()
	os.Exit(code)
}
