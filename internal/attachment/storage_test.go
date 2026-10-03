package attachment

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
)

type storageTransport func(*http.Request) (*http.Response, error)

func (f storageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func storageResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestStoragePrivacyProbe(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		anonymous int
		versioned bool
		corrupt   bool
		deleteErr bool
		invalid   bool
		wantError bool
	}{
		{name: "private", anonymous: http.StatusForbidden},
		{name: "versioned private", anonymous: http.StatusForbidden, versioned: true},
		{name: "not public", anonymous: http.StatusNotFound},
		{name: "public refused", anonymous: http.StatusOK, wantError: true},
		{name: "corrupt probe refused and removed", anonymous: http.StatusForbidden, corrupt: true, wantError: true},
		{name: "cleanup failure refused", anonymous: http.StatusForbidden, deleteErr: true, wantError: true},
		{name: "invalid config never sends", invalid: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Endpoint: "https://objects.example.test", Region: "test", Bucket: "private", AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret"}
			if test.invalid {
				cfg.Bucket = "../other"
			}
			var content []byte
			var methods []string
			transport := storageTransport(func(r *http.Request) (*http.Response, error) {
				methods = append(methods, r.Method)
				if !strings.HasPrefix(r.URL.Path, "/private/probe/att_") {
					t.Errorf("unexpected probe path %q", r.URL.Path)
				}
				if r.Header.Get("Authorization") == "" {
					if r.Method != http.MethodGet {
						t.Error("anonymous probe must read")
					}
					return storageResponse(test.anonymous, ""), nil
				}
				switch r.Method {
				case http.MethodPut:
					var err error
					content, err = io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					digest, err := hex.DecodeString(artifact.Digest(content))
					if err != nil {
						return nil, err
					}
					if r.Header.Get("If-None-Match") != "*" || r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(digest) {
						t.Error("probe omitted immutable write or content integrity condition")
					}
					return storageResponse(http.StatusOK, ""), nil
				case http.MethodGet:
					if test.corrupt {
						return storageResponse(http.StatusOK, "corrupt"), nil
					}
					return storageResponse(http.StatusOK, string(content)), nil
				case http.MethodHead:
					response := storageResponse(http.StatusOK, "")
					if test.versioned {
						response.Header.Set("X-Amz-Version-Id", "probe-version")
					}
					return response, nil
				case http.MethodDelete:
					if test.versioned && r.URL.Query().Get("versionId") != "probe-version" || !test.versioned && r.URL.Query().Get("versionId") != "" {
						t.Error("probe cleanup did not delete the observed object version")
					}
					if test.deleteErr {
						return storageResponse(http.StatusForbidden, `<Error><Code>AccessDenied</Code></Error>`), nil
					}
					content = nil
					return storageResponse(http.StatusNoContent, ""), nil
				default:
					return nil, errors.New("unexpected probe method")
				}
			})
			store, err := NewStorage(t.Context(), cfg, transport)
			if (err != nil) != test.wantError || (store == nil) != test.wantError {
				t.Fatalf("store=%v error=%v", store, err)
			}
			if test.invalid {
				if len(methods) != 0 {
					t.Fatal("invalid configuration reached storage")
				}
				return
			}
			want := []string{"PUT", "GET", "GET", "HEAD", "DELETE"}
			if test.corrupt {
				want = []string{"PUT", "GET", "HEAD", "DELETE"}
			}
			if !test.deleteErr && len(content) != 0 {
				t.Fatal("startup probe was not deleted")
			}
			if !reflect.DeepEqual(methods, want) {
				t.Fatalf("probe lifecycle=%v want %v", methods, want)
			}
		})
	}
}

func TestStorageWalkIsolation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, prefix, key, modified string
		visitorErr                  bool
		wantError                   bool
	}{
		{name: "scoped", prefix: "orgs/org_alpha/", key: "orgs/org_alpha/attachments/att_1", modified: "2026-01-01T00:00:00Z"},
		{name: "invalid prefix", prefix: "org_alpha", wantError: true},
		{name: "foreign object refused", prefix: "orgs/org_alpha/", key: "orgs/org_beta/attachments/att_1", modified: "2026-01-01T00:00:00Z", wantError: true},
		{name: "missing timestamp refused", prefix: "orgs/org_alpha/", key: "orgs/org_alpha/attachments/att_1", wantError: true},
		{name: "visitor error retained", prefix: "orgs/org_alpha/", key: "orgs/org_alpha/attachments/att_1", modified: "2026-01-01T00:00:00Z", visitorErr: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var listed, visited int
			transport := storageTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("list-type") == "2" {
					listed++
					if r.URL.Query().Get("prefix") != test.prefix {
						t.Error("listing lost organization prefix")
					}
					modified := ""
					if test.modified != "" {
						modified = "<LastModified>" + test.modified + "</LastModified>"
					}
					return storageResponse(http.StatusOK, "<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>"+test.key+"</Key>"+modified+"</Contents></ListBucketResult>"), nil
				}
				if r.Method == http.MethodGet && r.Header.Get("Authorization") == "" {
					return storageResponse(http.StatusForbidden, ""), nil
				}
				if r.Method == http.MethodGet {
					return storageResponse(http.StatusOK, "detent-private-attachments-probe"), nil
				}
				return storageResponse(http.StatusOK, ""), nil
			})
			store, err := NewStorage(t.Context(), Config{Endpoint: "https://objects.example.test", Region: "test", Bucket: "private", AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret"}, transport)
			if err != nil {
				t.Fatal(err)
			}
			visitorFailure := errors.New("visitor failed")
			err = store.Walk(t.Context(), test.prefix, func(object Object) error {
				visited++
				at, err := time.Parse(time.RFC3339, test.modified)
				if err != nil || object.Key != test.key || !object.CreatedAt.Equal(at) {
					t.Errorf("listed object=%+v", object)
				}
				if test.visitorErr {
					return visitorFailure
				}
				return nil
			})
			if (err != nil) != test.wantError || test.visitorErr && !errors.Is(err, visitorFailure) {
				t.Fatalf("walk error=%v", err)
			}
			if test.prefix == "org_alpha" && listed != 0 || test.wantError && !test.visitorErr && visited != 0 || !test.wantError && visited != 1 {
				t.Fatalf("listing=%d visits=%d", listed, visited)
			}
			if err := store.Put(t.Context(), "ignored", bytes.NewReader(nil), 0, "text/plain", "not-hex"); err == nil {
				t.Fatal("invalid checksum accepted")
			}
		})
	}
}
