package boardview

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestRespondCompressesWhenTheClientAcceptsGzip(t *testing.T) {
	t.Parallel()
	large := map[string]string{"title": strings.Repeat("example card ", 200)}
	small := map[string]string{"title": "example"}
	for _, test := range []struct {
		name   string
		accept string
		value  any
		gzip   bool
	}{
		{"browser encodings", "gzip, deflate, br, zstd", large, true},
		{"gzip with quality", "br;q=1.0, gzip;q=0.5", large, true},
		{"gzip refused", "gzip;q=0, br", large, false},
		{"no encoding", "", large, false},
		{"other encoding only", "br", large, false},
		{"small body", "gzip", small, true},
		{"identity refused", "gzip, identity;q=0", small, true},
		{"wildcard", "*;q=1, identity;q=0", large, true},
		{"wildcard with gzip refused", "*, gzip;q=0", large, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, "/work-board", nil)
			if test.accept != "" {
				request.Header.Set("Accept-Encoding", test.accept)
			}
			recorder := httptest.NewRecorder()
			if err := Respond(recorder, request, http.StatusOK, test.value); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" || recorder.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatalf("status %d headers %v", recorder.Code, recorder.Header())
			}
			if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(recorder.Body.Len()) {
				t.Fatalf("Content-Length = %s, body has %d bytes", got, recorder.Body.Len())
			}
			body := recorder.Body.Bytes()
			if compressed := recorder.Header().Get("Content-Encoding") == "gzip"; compressed != test.gzip {
				t.Fatalf("Content-Encoding = %q, want gzip %v", recorder.Header().Get("Content-Encoding"), test.gzip)
			}
			if test.gzip {
				reader, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				if body, err = io.ReadAll(reader); err != nil {
					t.Fatal(err)
				}
			}
			want, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bytes.TrimSuffix(body, []byte("\n")), want) {
				t.Fatalf("decoded body differs from the JSON encoding of the value")
			}
		})
	}
}
