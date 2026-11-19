package stoptest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func ReadEvent(t *testing.T, request *http.Request) tracker.NativeRunEvent {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	var event tracker.NativeRunEvent
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatal(err)
	}
	return event
}
