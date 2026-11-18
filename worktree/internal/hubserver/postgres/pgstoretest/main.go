package pgstoretest

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func Main(m *testing.M) {
	flag.Parse()
	timeout, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil || timeout <= 0 {
		fmt.Fprintln(os.Stderr, "PostgreSQL schema tests require a nonzero go test -timeout")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		cancel()
		os.Exit(1)
	}
	code := m.Run()
	if err := Template.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	cancel()
	os.Exit(code)
}
