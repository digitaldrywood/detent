package staticasset

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestHandlerServesNegotiatedCachedRepresentations(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"app/conversation/app.js":                     {Data: []byte("entry")},
		"app/conversation/app.js.br":                  {Data: []byte("entry-br")},
		"app/conversation/app.js.gz":                  {Data: []byte("entry-gz")},
		"app/conversation/chunks/Work-AbCd1234.js":    {Data: []byte("chunk")},
		"app/conversation/chunks/Work-AbCd1234.js.gz": {Data: []byte("chunk-gz")},
		"app/conversation/favicon-32.png":             {Data: []byte("png")},
		"fonts/Geist-Variable.woff2":                  {Data: []byte("font")},
		"app/conversation/app.css":                    {Data: []byte("body{}")},
		"app/conversation/app.js.map":                 {Data: []byte("{}")},
		"app/conversation/sniffed.detentunknown":      {Data: []byte("\x89PNG\r\n\x1a\nrest")},
	}
	for _, test := range []struct {
		name, path, accept string
		status             int
		body, encoding     string
		cache, vary, ctype string
	}{
		{name: "brotli preferred", path: "/app/conversation/app.js", accept: "gzip, deflate, br", status: http.StatusOK, body: "entry-br", encoding: "br", cache: RevalidateCacheControl, vary: "Accept-Encoding", ctype: "text/javascript; charset=utf-8"},
		{name: "gzip when brotli refused", path: "/app/conversation/app.js", accept: "br;q=0, gzip", status: http.StatusOK, body: "entry-gz", encoding: "gzip", cache: RevalidateCacheControl, vary: "Accept-Encoding", ctype: "text/javascript; charset=utf-8"},
		{name: "wildcard admits brotli", path: "/app/conversation/app.js", accept: "*", status: http.StatusOK, body: "entry-br", encoding: "br", cache: RevalidateCacheControl, vary: "Accept-Encoding", ctype: "text/javascript; charset=utf-8"},
		{name: "identity without accept", path: "/app/conversation/app.js", status: http.StatusOK, body: "entry", cache: RevalidateCacheControl, vary: "Accept-Encoding", ctype: "text/javascript; charset=utf-8"},
		{name: "hashed chunk immutable", path: "/app/conversation/chunks/Work-AbCd1234.js", accept: "br, gzip", status: http.StatusOK, body: "chunk-gz", encoding: "gzip", cache: ImmutableCacheControl, vary: "Accept-Encoding", ctype: "text/javascript; charset=utf-8"},
		{name: "uncompressed file has no vary", path: "/app/conversation/favicon-32.png", accept: "br", status: http.StatusOK, body: "png", cache: RevalidateCacheControl, ctype: "image/png"},
		{name: "dashboard font revalidated", path: "/fonts/Geist-Variable.woff2", status: http.StatusOK, body: "font", cache: RevalidateCacheControl, ctype: "font/woff2"},
		{name: "stylesheet", path: "/app/conversation/app.css", status: http.StatusOK, body: "body{}", cache: RevalidateCacheControl, ctype: "text/css; charset=utf-8"},
		{name: "source map", path: "/app/conversation/app.js.map", status: http.StatusOK, body: "{}", cache: RevalidateCacheControl, ctype: "application/json"},
		{name: "unknown extension sniffed", path: "/app/conversation/sniffed.detentunknown", status: http.StatusOK, body: "\x89PNG\r\n\x1a\nrest", cache: RevalidateCacheControl, ctype: "image/png"},
		{name: "missing file", path: "/app/conversation/chunks/Gone-AbCd1234.js", status: http.StatusNotFound},
		{name: "directory", path: "/app/conversation/chunks", status: http.StatusNotFound},
		{name: "traversal", path: "/../app/conversation/app.js", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.URL.Path = test.path
			if test.accept != "" {
				request.Header.Set("Accept-Encoding", test.accept)
			}
			response := httptest.NewRecorder()
			New(files).ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if test.status != http.StatusOK {
				return
			}
			header := response.Header()
			for name, want := range map[string]string{"Content-Encoding": test.encoding, "Cache-Control": test.cache, "Vary": test.vary, "Content-Type": test.ctype} {
				if got := header.Get(name); got != want {
					t.Fatalf("%s = %q, want %q", name, got, want)
				}
			}
			if got := response.Body.String(); got != test.body {
				t.Fatalf("body = %q, want %q", got, test.body)
			}
			if header.Get("ETag") == "" {
				t.Fatal("response has no ETag")
			}
		})
	}
}

func TestHandlerRevalidatesByRepresentationETag(t *testing.T) {
	t.Parallel()
	handler := New(fstest.MapFS{
		"app/conversation/app.js":    {Data: []byte("entry")},
		"app/conversation/app.js.br": {Data: []byte("entry-br")},
	})
	get := func(accept, match string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/app/conversation/app.js", nil)
		request.Header.Set("Accept-Encoding", accept)
		if match != "" {
			request.Header.Set("If-None-Match", match)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	brotli, identity := get("br", "").Header().Get("ETag"), get("", "").Header().Get("ETag")
	if brotli == identity {
		t.Fatalf("brotli and identity share ETag %s", brotli)
	}
	if response := get("br", brotli); response.Code != http.StatusNotModified || response.Body.Len() != 0 {
		t.Fatalf("matching revalidation = %d with %d bytes, want 304 and none", response.Code, response.Body.Len())
	}
	if response := get("", brotli); response.Code != http.StatusOK || response.Body.String() != "entry" {
		t.Fatalf("identity with brotli ETag = %d %q, want the identity body", response.Code, response.Body.String())
	}
}
