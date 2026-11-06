package testsupport

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/testenv"
)

type RoundTrip func(*http.Request) (*http.Response, error)

func (f RoundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
func HandlerClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: RoundTrip(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		incoming := request.Clone(request.Context())
		incoming.RequestURI = incoming.URL.RequestURI()
		handler.ServeHTTP(recorder, incoming)
		return recorder.Result(), nil
	})}
}

var DatabasePath = testenv.DatabaseTemplate(func(ctx context.Context, path string) (io.Closer, error) {
	return hubserver.Open(ctx, hubserver.Config{DatabasePath: path, GitHubDisabled: true, Logger: slog.New(slog.DiscardHandler)})
})
