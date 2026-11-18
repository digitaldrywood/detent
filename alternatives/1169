package api

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestNativeCommentOffsetRefusal(t *testing.T) {
	client := NativeClient{project: "prj_example"}
	for _, offset := range []int{1, 100} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			arguments, err := json.Marshal(map[string]any{"project_id": "prj_example", "reference": "wi_example", "offset": offset, "limit": 100})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ReadAgentTool(t.Context(), runner.AgentToolCall{Name: operatortool.WorkComments, Arguments: arguments}, time.Now())
			var refusal *operatortool.RequestError
			if !errors.As(err, &refusal) || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, "returned next_cursor") {
				t.Fatalf("native offset refusal lacks paging guidance: %v", err)
			}
		})
	}
}
