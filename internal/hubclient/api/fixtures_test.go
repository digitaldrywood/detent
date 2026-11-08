package api

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/testenv"
)

type executionRoundTrip = testsupport.RoundTrip

var providerHandlerClient = testsupport.HandlerClient
var newHubContentStore = testsupport.ContentStore

const nativeChangeAdminToken = "native-change-admin"

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	code := m.Run()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := testsupport.CloseDispatchPostgres(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	cancel()
	os.Exit(code)
}
