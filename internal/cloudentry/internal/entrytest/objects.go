package entrytest

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

type SpacesObject struct {
	Content []byte
	Created time.Time
}

type Spaces struct {
	Mu              sync.Mutex
	Objects         map[string]SpacesObject
	Keys            []string
	Public          bool
	FailDelete      bool
	FailPutResponse bool
	DenyArchive     bool
	Transport       http.RoundTripper
}

func NewSpaces(t *testing.T, public bool) *Spaces {
	t.Helper()
	f := &Spaces{Objects: map[string]SpacesObject{}, Public: public}
	f.Transport = HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Mu.Lock()
		defer f.Mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/private/")
		if r.Header.Get("Authorization") == "" && !f.Public {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if f.DenyArchive && (r.URL.Query().Has("lifecycle") || r.URL.Query().Has("cors")) {
			w.WriteHeader(http.StatusForbidden)
			if _, err := io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Query().Has("lifecycle") {
			w.WriteHeader(http.StatusNotFound)
			_, err := io.WriteString(w, `<Error><Code>NoSuchLifecycleConfiguration</Code></Error>`)
			if err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Query().Has("cors") && r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/private/") && r.URL.Path != "/private" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			type item struct {
				Key          string
				LastModified string
				Size         int
			}
			result := struct {
				XMLName   xml.Name `xml:"ListBucketResult"`
				Truncated bool     `xml:"IsTruncated"`
				Items     []item   `xml:"Contents"`
			}{}
			var keys []string
			for key := range f.Objects {
				if strings.HasPrefix(key, r.URL.Query().Get("prefix")) {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			for _, key := range keys {
				object := f.Objects[key]
				result.Items = append(result.Items, item{Key: key, LastModified: object.Created.UTC().Format(time.RFC3339), Size: len(object.Content)})
			}
			if err := xml.NewEncoder(w).Encode(result); err != nil {
				t.Error(err)
			}
			return
		}
		if r.Method == http.MethodGet && r.URL.Query().Has("versioning") {
			w.Header().Set("Content-Type", "application/xml")
			if _, err := io.WriteString(w, `<VersioningConfiguration/>`); err != nil {
				t.Error(err)
			}
			return
		}
		f.Keys = append(f.Keys, r.Method+" "+key)
		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("Content-Type") != "application/gzip" && r.Header.Get("If-None-Match") != "*" {
				t.Error("upload lacks conditional put")
			}
			content, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			if previous, exists := f.Objects[key]; exists && (r.Header.Get("Content-Type") != "application/gzip" || !bytes.Equal(content, previous.Content)) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			f.Objects[key] = SpacesObject{Content: content, Created: time.Now()}
			if f.FailPutResponse {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("ETag", `"test"`)
		case http.MethodGet:
			object, ok := f.Objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if _, err := w.Write(object.Content); err != nil {
				t.Error(err)
			}
		case http.MethodHead:
			object, ok := f.Objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Last-Modified", object.Created.UTC().Format(http.TimeFormat))
		case http.MethodDelete:
			if f.FailDelete {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			delete(f.Objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})}
	return f
}

func (f *Spaces) Config() attachment.Config {
	return attachment.Config{Endpoint: "http://127.0.0.1", Region: "test", Bucket: "private", AccessKeyID: "test-key", SecretAccessKey: "test-secret"}
}

func (f *Spaces) Requests() []string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]string{}, f.Keys...)
}

type ConversationArchive struct {
	Mu      sync.Mutex
	Content map[string][]byte
}

func (f *ConversationArchive) HandleContent(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.Reference.Validate(request.Organization); err != nil {
		return nil, err
	}
	if !request.Write {
		return f.ReadContent(ctx, request.Organization, request.Reference)
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Content == nil {
		f.Content = map[string][]byte{}
	}
	f.Content[request.Reference.Key] = bytes.Clone(request.Data)
	return nil, nil
}

func (f *ConversationArchive) PublishContent(_ context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Content == nil {
		f.Content = map[string][]byte{}
	}
	f.Content[ref.Key] = bytes.Clone(content)
	return ref, nil
}

func (f *ConversationArchive) ReadContent(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	content, ok := f.Content[ref.Key]
	if !ok {
		return nil, errors.New("archive content missing")
	}
	return bytes.Clone(content), nil
}
