package attachment

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestArchiveStorageLifecycle(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		fail, corrupt bool
	}{
		{name: "verified and deduplicated"},
		{name: "upload failure", fail: true},
		{name: "verification failure", corrupt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := map[string][]byte{}
			puts := map[string]int{}
			transport := storageTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") == "" {
					return storageResponse(http.StatusForbidden, ""), nil
				}
				key := r.URL.Path
				switch r.Method {
				case http.MethodPut:
					archive := strings.Contains(key, "/blobs/")
					if archive && test.fail {
						return storageResponse(http.StatusBadRequest, `<Error><Code>InvalidRequest</Code><Message>fixture-secret</Message></Error>`), nil
					}
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					if archive {
						if r.Header.Get("Content-Type") != "application/gzip" || r.Header.Get("Content-Encoding") != "" || r.Header.Get("If-None-Match") != "" || r.Header.Get("X-Amz-Checksum-Sha256") != "" {
							t.Error("archive PUT requires only the Spaces portable subset")
						}
						if test.corrupt {
							data = []byte("corrupt")
						}
					}
					objects[key] = data
					puts[key]++
					return storageResponse(http.StatusOK, ""), nil
				case http.MethodGet:
					data, ok := objects[key]
					if !ok {
						return storageResponse(http.StatusNotFound, `<Error><Code>NoSuchKey</Code></Error>`), nil
					}
					return storageResponse(http.StatusOK, string(data)), nil
				case http.MethodHead:
					return storageResponse(http.StatusOK, ""), nil
				case http.MethodDelete:
					delete(objects, key)
					return storageResponse(http.StatusNoContent, ""), nil
				}
				t.Fatalf("unexpected request %s", r.Method)
				return storageResponse(http.StatusMethodNotAllowed, ""), nil
			})
			cfg := Config{Endpoint: "https://objects.example.test", Region: "nyc3", Bucket: "private", AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret"}
			store, err := NewStorage(t.Context(), cfg, transport)
			if err != nil {
				t.Fatal(err)
			}
			content := strings.Repeat("archived detail\n", 1000)
			refs := make([]ArchiveReference, 0, 2)
			for _, org := range []string{"org_alpha", "org_beta"} {
				ref, data, err := PrepareArchive(org, 1, strings.NewReader(content))
				if err != nil {
					t.Fatal(err)
				}
				other, repeated, err := PrepareArchive(org, 1, strings.NewReader(content))
				if err != nil || ref != other || !bytes.Equal(data, repeated) {
					t.Fatal("archive encoding is not deterministic")
				}
				reader := bytes.NewReader(data)
				gz, err := gzip.NewReader(reader)
				if err != nil {
					t.Fatal(err)
				}
				gz.Multistream(false)
				decoded, err := io.ReadAll(gz)
				if err != nil || gz.Close() != nil || string(decoded) != content || reader.Len() != 0 || !gz.ModTime.IsZero() || gz.Name != "" || gz.Comment != "" {
					t.Fatal("archive is not a single deterministic gzip member")
				}
				err = StoreArchive(t.Context(), store.(ArchiveStorage), org, ref, data)
				if test.fail || test.corrupt {
					if err == nil || strings.Contains(err.Error(), "fixture-secret") {
						t.Fatalf("unsafe storage error: %v", err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := StoreArchive(t.Context(), store.(ArchiveStorage), org, ref, data); err != nil {
					t.Fatal(err)
				}
				if puts["/private/"+ref.Key] != 1 {
					t.Fatal("identical content uploaded twice")
				}
				refs = append(refs, ref)
				access, err := store.(ArchiveStorage).PresignArchive(t.Context(), ref)
				if err != nil {
					t.Fatal(err)
				}
				signed := access.URL
				u, err := url.Parse(signed)
				if err != nil {
					t.Fatal(err)
				}
				q := u.Query()
				signedAt, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
				if err != nil || !access.ExpiresAt.Equal(signedAt.Add(5*time.Minute)) {
					t.Fatal("expiry disagrees with the signature")
				}
				if u.Path != "/private/"+ref.Key || q.Get("X-Amz-Expires") != "300" || q.Get("X-Amz-SignedHeaders") != "host" || q.Get("versionId") != "" || strings.Contains(signed, cfg.SecretAccessKey) {
					t.Fatal("presigned read lost its key, expiry or privacy")
				}
				if archiveSignature(u, http.MethodGet, cfg.SecretAccessKey) != q.Get("X-Amz-Signature") || archiveSignature(u, http.MethodHead, cfg.SecretAccessKey) == q.Get("X-Amz-Signature") {
					t.Fatal("signature is not scoped to GET")
				}
				var logs bytes.Buffer
				slog.New(slog.NewTextHandler(&logs, nil)).Info("read", "archive", access)
				if strings.Contains(logs.String(), signed) || strings.Contains(logs.String(), "X-Amz-") {
					t.Fatal("presigned URL leaked into logs")
				}
				owner := ArchiveContents{Storage: store.(ArchiveStorage)}
				readContent, err := owner.ReadContent(t.Context(), org, ref)
				if err != nil || string(readContent) != content {
					t.Fatalf("archive content read: %v", err)
				}
				published, err := owner.PublishContent(t.Context(), org, []byte(content))
				if err != nil || published != ref {
					t.Fatalf("content publication changed reference: %v", err)
				}
				wrongOrg := "org_other"
				if _, err := owner.ReadContent(t.Context(), wrongOrg, ref); err == nil {
					t.Fatal("cross-organization archive read accepted")
				}
				encoded, err := json.Marshal(ref)
				if err != nil || strings.Contains(string(encoded), cfg.SecretAccessKey) || strings.Contains(string(encoded), "X-Amz-") {
					t.Fatal("credentials leaked into reference")
				}
			}
			if !test.fail && !test.corrupt && (refs[0].Key == refs[1].Key || refs[0].SHA256 != refs[1].SHA256 || len(objects) != 2) {
				t.Fatal("content deduplicated across organizations")
			}
		})
	}
}

func archiveSignature(u *url.URL, method, secret string) string {
	q := u.Query()
	q.Del("X-Amz-Signature")
	canonical := method + "\n" + u.EscapedPath() + "\n" + q.Encode() + "\nhost:" + u.Host + "\n\nhost\nUNSIGNED-PAYLOAD"
	digest := sha256.Sum256([]byte(canonical))
	credential := strings.Split(q.Get("X-Amz-Credential"), "/")
	scope := strings.Join(credential[1:], "/")
	key := []byte("AWS4" + secret)
	for _, component := range credential[1:] {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(component))
		key = h.Sum(nil)
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte("AWS4-HMAC-SHA256\n" + q.Get("X-Amz-Date") + "\n" + scope + "\n" + hex.EncodeToString(digest[:])))
	return hex.EncodeToString(h.Sum(nil))
}

func TestArchiveBucketPolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, rule string
		rejected   bool
	}{
		{name: "no lifecycle"},
		{name: "global expiry", rule: `<Status>Enabled</Status><Expiration><Days>30</Days></Expiration>`, rejected: true},
		{name: "archive expiry", rule: `<Status>Enabled</Status><Filter><Prefix>orgs/org_alpha/blobs/</Prefix></Filter><Expiration><Days>30</Days></Expiration>`, rejected: true},
		{name: "archive noncurrent expiry", rule: `<Status>Enabled</Status><Filter><And><Prefix>orgs/org_alpha/</Prefix><Tag><Key>kind</Key><Value>archive</Value></Tag></And></Filter><NoncurrentVersionExpiration><NoncurrentDays>30</NoncurrentDays></NoncurrentVersionExpiration>`, rejected: true},
		{name: "attachment expiry preserved", rule: `<Status>Enabled</Status><Prefix>orgs/org_alpha/attachments/</Prefix><Expiration><Days>30</Days></Expiration>`},
		{name: "disabled expiry", rule: `<Status>Disabled</Status><Expiration><Days>30</Days></Expiration>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			cors := false
			transport := storageTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Has("lifecycle") {
					if test.rule == "" {
						return storageResponse(http.StatusNotFound, `<Error><Code>NoSuchLifecycleConfiguration</Code></Error>`), nil
					}
					return storageResponse(http.StatusOK, `<LifecycleConfiguration><Rule>`+test.rule+`</Rule></LifecycleConfiguration>`), nil
				}
				if r.URL.Query().Has("cors") {
					var config struct {
						Rules []struct {
							Origins []string `xml:"AllowedOrigin"`
							Methods []string `xml:"AllowedMethod"`
							Exposed []string `xml:"ExposeHeader"`
						} `xml:"CORSRule"`
					}
					if err := xml.NewDecoder(r.Body).Decode(&config); err != nil {
						t.Fatal(err)
					}
					if len(config.Rules) != 1 || !reflect.DeepEqual(config.Rules[0].Origins, []string{"https://app.example.test"}) || !reflect.DeepEqual(config.Rules[0].Methods, []string{"GET", "HEAD"}) || !reflect.DeepEqual(config.Rules[0].Exposed, []string{"Content-Length", "Content-Type", "ETag"}) {
						t.Fatal("bucket CORS lost exact origins or read-only permissions")
					}
					cors = true
					return storageResponse(http.StatusOK, ""), nil
				}
				if r.Method == http.MethodGet && r.Header.Get("Authorization") == "" {
					return storageResponse(http.StatusForbidden, ""), nil
				}
				if r.Method == http.MethodGet {
					return storageResponse(http.StatusOK, "detent-private-attachments-probe"), nil
				}
				return storageResponse(http.StatusOK, ""), nil
			})
			store, err := NewStorage(t.Context(), Config{Endpoint: "https://objects.example.test", Region: "nyc3", Bucket: "private", AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret"}, transport)
			if err != nil {
				t.Fatal(err)
			}
			err = store.(ArchiveStorage).ConfigureArchiveReads(t.Context(), []string{"https://app.example.test"})
			if (err != nil) != test.rejected || cors == test.rejected {
				t.Fatalf("policy error=%v CORS=%t", err, cors)
			}
			if err := store.(ArchiveStorage).ConfigureArchiveReads(t.Context(), []string{"*"}); err == nil {
				t.Fatal("wildcard origin accepted")
			}
		})
	}
}

func TestDecodeArchiveSection(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		mutate    func(*ArchiveReference, *[]byte)
		valid     bool
	}{
		{"valid", `{"schema_version":1,"organization_id":"org_example","project_id":"prj_example","work_item_id":"wi_example","section_id":"run_example","tables":[]}`, nil, true},
		{"unknown schema", `{"schema_version":2,"organization_id":"org_example"}`, nil, false},
		{"foreign organization", `{"schema_version":1,"organization_id":"org_other"}`, nil, false},
		{"unknown field", `{"schema_version":1,"organization_id":"org_example","url":"http://169.254.169.254/"}`, nil, false},
		{"trailing JSON", `{"schema_version":1,"organization_id":"org_example"}{}`, nil, false},
		{"uncompressed limit", `{"schema_version":1,"organization_id":"org_example"}`, func(ref *ArchiveReference, _ *[]byte) { ref.UncompressedSize = MaxSectionBytes + 1 }, false},
		{"incorrect size", `{"schema_version":1,"organization_id":"org_example"}`, func(ref *ArchiveReference, _ *[]byte) { ref.UncompressedSize++ }, false},
		{"digest mismatch", `{"schema_version":1,"organization_id":"org_example"}`, func(_ *ArchiveReference, data *[]byte) { (*data)[0] ^= 1 }, false},
		{"second gzip member", `{"schema_version":1,"organization_id":"org_example"}`, func(ref *ArchiveReference, data *[]byte) {
			*data = append(*data, *data...)
			ref.CompressedSize = int64(len(*data))
			digest := sha256.Sum256(*data)
			ref.SHA256 = hex.EncodeToString(digest[:])
			ref.Key = "orgs/org_example/blobs/sha256/" + ref.SHA256[:2] + "/" + ref.SHA256
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ref, data, err := PrepareArchive("org_example", 1, strings.NewReader(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(&ref, &data)
			}
			_, err = DecodeArchiveSection("org_example", ref, data)
			if (err == nil) != test.valid {
				t.Fatalf("decode error=%v valid=%v", err, test.valid)
			}
		})
	}
}
