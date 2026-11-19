package projectadmin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestToolLinksPreserveNonBrowserContent(t *testing.T) {
	t.Parallel()
	public := "http://127.0.0.1:7777"
	links := ReadableLinks{PublicURL: &public}
	projection, err := links.ToolResult(t.Context(), operatortool.Result{Content: json.RawMessage(`{"sequence":9223372036854775807,"url":"https://github.com/example/repo/issues/wi_external","manifest_url":"/api/v2/work-items/wi_external/download"}`)}, operatortool.WorkItem)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"sequence":9223372036854775807`, `"url":"https://github.com/example/repo/issues/wi_external"`, `"manifest_url":"/api/v2/work-items/wi_external/download"`} {
		if !strings.Contains(string(projection.Content), want) {
			t.Fatalf("projection lost %s: %s", want, projection.Content)
		}
	}
}
