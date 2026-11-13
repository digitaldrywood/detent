package workitemchange_test

import (
	"encoding/json"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func testJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
