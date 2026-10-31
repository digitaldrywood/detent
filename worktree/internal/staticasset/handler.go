// Package staticasset serves the embedded static tree with the cache and
// content-encoding policy of the Cloud application client.
//
// The client build names every file under app/conversation/chunks and
// app/conversation/assets after its content hash, so those files never change
// under their URL and are cached as immutable. Every other file keeps a stable
// name across builds (the entry app.js, app.css, fonts, images) and is
// revalidated against its ETag on each use, so a deploy is visible on the next
// load. The build writes .br and .gz siblings next to each compressible file;
// a request that accepts one of those encodings is answered with it.
package staticasset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ImmutableCacheControl  = "public, max-age=31536000, immutable"
	RevalidateCacheControl = "no-cache"
)

var hashedPrefixes = []string{"app/conversation/chunks/", "app/conversation/assets/"}

type encoding struct {
	token     string
	extension string
}

var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".wasm":  "application/wasm",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

var encodings = []encoding{{token: "br", extension: ".br"}, {token: "gzip", extension: ".gz"}}

// Handler serves files from an fs.FS rooted at the static tree. Requests carry
// the path relative to that root, so mount it behind http.StripPrefix.
type Handler struct {
	files fs.FS
	mu    sync.Mutex
	tags  map[string]string
}

func New(files fs.FS) *Handler {
	return &Handler{files: files, tags: map[string]string{}}
}

// Immutable reports whether a static path names a content-hashed file.
func Immutable(name string) bool {
	for _, prefix := range hashedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if !fs.ValidPath(name) || name == "." {
		http.NotFound(w, r)
		return
	}
	info, err := fs.Stat(h.files, name)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	served, coding, variants := h.representation(name, r.Header.Get("Accept-Encoding"))
	content, tag, err := h.open(served)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = content.Close() }() //nolint:errcheck // The response is already written; a close failure has no reader.
	header := w.Header()
	if variants {
		header.Add("Vary", "Accept-Encoding")
	}
	if coding != "" {
		header.Set("Content-Encoding", coding)
	}
	header.Set("Content-Type", h.contentType(name))
	header.Set("ETag", tag)
	if Immutable(name) {
		header.Set("Cache-Control", ImmutableCacheControl)
	} else {
		header.Set("Cache-Control", RevalidateCacheControl)
	}
	http.ServeContent(w, r, "", time.Time{}, content)
}

func (h *Handler) representation(name, accept string) (string, string, bool) {
	served, coding, variants := name, "", false
	for _, candidate := range encodings {
		if _, err := fs.Stat(h.files, name+candidate.extension); err != nil {
			continue
		}
		variants = true
		if coding == "" && accepts(accept, candidate.token) {
			served, coding = name+candidate.extension, candidate.token
		}
	}
	return served, coding, variants
}

// contentType names a file's type from the extensions the client build emits,
// independent of the host's mime.types, then from the system table, and last
// by sniffing the uncompressed bytes.
func (h *Handler) contentType(name string) string {
	extension := strings.ToLower(path.Ext(name))
	if known, ok := contentTypes[extension]; ok {
		return known
	}
	if system := mime.TypeByExtension(extension); system != "" {
		return system
	}
	file, err := h.files.Open(name)
	if err != nil {
		return "application/octet-stream"
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // Only the sniffed prefix matters.
	prefix := make([]byte, 512)
	read, _ := io.ReadFull(file, prefix) //nolint:errcheck // A short file sniffs from what it has.
	return http.DetectContentType(prefix[:read])
}

type seekableFile struct {
	io.ReadSeeker
	close func() error
}

func (f seekableFile) Close() error { return f.close() }

func (h *Handler) open(name string) (seekableFile, string, error) {
	file, err := h.files.Open(name)
	if err != nil {
		return seekableFile{}, "", err
	}
	tag, err := h.tag(name)
	if err != nil {
		return seekableFile{}, "", errors.Join(err, file.Close())
	}
	if seeker, ok := file.(io.ReadSeeker); ok {
		return seekableFile{ReadSeeker: seeker, close: file.Close}, tag, nil
	}
	data, err := io.ReadAll(file)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return seekableFile{}, "", err
	}
	return seekableFile{ReadSeeker: bytes.NewReader(data), close: func() error { return nil }}, tag, nil
}

func (h *Handler) tag(name string) (string, error) {
	h.mu.Lock()
	cached, ok := h.tags[name]
	h.mu.Unlock()
	if ok {
		return cached, nil
	}
	data, err := fs.ReadFile(h.files, name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	tag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h.mu.Lock()
	h.tags[name] = tag
	h.mu.Unlock()
	return tag, nil
}

// accepts reports whether an Accept-Encoding value admits a coding with a
// non-zero quality, explicitly or through "*".
func accepts(header, coding string) bool {
	wildcard := false
	for part := range strings.SplitSeq(header, ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		token = strings.ToLower(strings.TrimSpace(token))
		if token != coding && token != "*" {
			continue
		}
		quality := 1.0
		for param := range strings.SplitSeq(params, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					quality = parsed
				}
			}
		}
		if token == coding {
			return quality > 0
		}
		wildcard = quality > 0
	}
	return wildcard
}
