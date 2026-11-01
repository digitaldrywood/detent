package api

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/testenv"
)

type executionRoundTrip = testsupport.RoundTrip

var providerHandlerClient = testsupport.HandlerClient
var hubDatabasePath = testsupport.DatabasePath
var newHubContentStore = testsupport.ContentStore

const nativeChangeAdminToken = "native-change-admin"

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
