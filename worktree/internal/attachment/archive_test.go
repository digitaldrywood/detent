package attachment

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"
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
				decodedDetail, readErr := ReadArchive(t.Context(), store, org, ref)
				if readErr != nil || string(decodedDetail) != content {
					t.Fatalf("read detail: %v", readErr)
				}
				foreign := "org_foreign"
				if _, err := ReadArchive(t.Context(), store, foreign, ref); err == nil {
					t.Fatal("foreign archive was readable")
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

func TestArchiveBindingEndpoints(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		endpoint string
		allowed  bool
	}{
		{"https://1.1.1.1", true},
		{"http://1.1.1.1", false},
		{"https://localhost", false},
		{"https://LOCALHOST.", false},
		{"https://metadata.localhost", false},
		{"https://127.0.0.1", false},
		{"https://169.254.169.254", false},
		{"https://10.0.0.1", false},
		{"https://172.16.0.1", false},
		{"https://192.168.1.1", false},
		{"https://100.100.100.200", false},
		{"https://[::1]", false},
		{"https://[::ffff:127.0.0.1]", false},
		{"https://[fd00::1]", false},
		{"https://[fe80::1]", false},
		{"https://[64:ff9b::a00:1]", false},
		{"https://user:secret@1.1.1.1", false},
		{"https://1.1.1.1/?credential=secret", false},
	} {
		t.Run(test.endpoint, func(t *testing.T) {
			if err := ValidateArchiveEndpoint(t.Context(), test.endpoint); (err == nil) != test.allowed {
				t.Fatalf("endpoint allowed=%t error=%v", test.allowed, err)
			}
		})
	}
	for _, address := range []string{"0.0.0.0", "224.0.0.1", "198.18.0.1", "192.0.0.1", "2002:7f00:1::1"} {
		if publicArchiveAddress(netip.MustParseAddr(address)) {
			t.Fatalf("unsafe address allowed: %s", address)
		}
	}
	b := StorageBinding{Organization: "org_alpha", Backend: "s3", Endpoint: "https://objects.example.test", Region: "region", Bucket: "private", CredentialReference: "fixture-reference"}
	requests := 0
	transport := storageTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "objects.example.test" {
			t.Fatal("redirect destination contacted")
		}
		response := storageResponse(http.StatusFound, "")
		response.Header.Set("Location", "https://169.254.169.254/credentials")
		return response, nil
	})
	store, err := NewBindingStorage(t.Context(), b, Credentials{AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureArchiveReads(t.Context(), []string{"https://app.example.test"}); err == nil || requests != 1 {
		t.Fatalf("redirect accepted: requests=%d error=%v", requests, err)
	}
	guard, err := archiveTransport("https://objects.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if connection, err := guard.DialContext(t.Context(), "tcp", "127.0.0.1:443"); err == nil {
		connection.Close()
		t.Fatal("dial destination escaped endpoint")
	}
	var logs bytes.Buffer
	secret := Credentials{AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret"}
	raw, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	slog.New(slog.NewTextHandler(&logs, nil)).Info("binding", "credentials", secret, "binding", b)
	for _, value := range []string{"fixture-access", "fixture-secret", "fixture-reference"} {
		if strings.Contains(string(raw)+logs.String(), value) {
			t.Fatal("credentials leaked")
		}
	}
	lookups := map[string]bool{}
	var previous Credentials
	for _, organization := range []string{"org_alpha", "org_beta"} {
		secret, err := CredentialsFromEnvironment(organization, "shared_reference", func(key string) string {
			if lookups[key] {
				t.Fatal("credential reference crossed tenant boundary")
			}
			lookups[key] = true
			return key
		})
		if err != nil || secret.AccessKeyID == "" || secret.SecretAccessKey == "" || secret == previous {
			t.Fatal("credential reference did not resolve per organization")
		}
		previous = secret
	}
	if _, err := CredentialsFromEnvironment("org_alpha", "missing", func(string) string { return "" }); err == nil {
		t.Fatal("missing credentials accepted")
	}
}

type archiveResponseBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *archiveResponseBody) Read(data []byte) (int, error) {
	n, err := b.Reader.Read(data)
	b.read += n
	return n, err
}

func (b *archiveResponseBody) Close() error { b.closed = true; return nil }

func TestArchiveBindingResponseLimits(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, operation, query, xml       string
		status                            int
		knownLength, oversized, versioned bool
	}{
		{name: "versioning at limit", operation: "versioning", query: "versioning", xml: `<VersioningConfiguration/>`, status: http.StatusOK},
		{name: "versioning oversized", operation: "versioning", query: "versioning", xml: `<VersioningConfiguration/>`, status: http.StatusOK, oversized: true},
		{name: "lifecycle oversized", operation: "configure", query: "lifecycle", xml: `<LifecycleConfiguration/>`, status: http.StatusOK, oversized: true},
		{name: "cors oversized", operation: "configure", query: "cors", xml: `<Result/>`, status: http.StatusOK, oversized: true},
		{name: "listing oversized", operation: "erase", query: "list-type", xml: `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`, status: http.StatusOK, oversized: true},
		{name: "version listing oversized", operation: "erase", query: "versions", xml: `<ListVersionsResult><IsTruncated>false</IsTruncated></ListVersionsResult>`, status: http.StatusOK, oversized: true, versioned: true},
		{name: "error oversized", operation: "versioning", query: "versioning", xml: `<Error><Code>AccessDenied</Code><Message>fixture-secret</Message></Error>`, status: http.StatusForbidden, oversized: true},
		{name: "advertised oversized", operation: "versioning", query: "versioning", xml: `<VersioningConfiguration/>`, status: http.StatusOK, oversized: true, knownLength: true},
		{name: "object above control limit", operation: "object", status: http.StatusOK},
		{name: "object oversized", operation: "object", status: http.StatusOK, oversized: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			limit := 1 << 20
			if test.operation == "object" {
				limit = MaxBytes
			}
			size := limit
			if test.oversized {
				size += 1 << 16
			}
			var bodies []*archiveResponseBody
			transport := storageTransport(func(r *http.Request) (*http.Response, error) {
				if test.operation == "object" || r.URL.Query().Has(test.query) {
					body := &archiveResponseBody{Reader: io.MultiReader(strings.NewReader(test.xml), io.LimitReader(strings.NewReader(strings.Repeat(" ", size)), int64(size-len(test.xml))))}
					bodies = append(bodies, body)
					response := storageResponse(test.status, "")
					response.Body = body
					response.ContentLength = -1
					if test.knownLength {
						response.ContentLength = int64(size)
					}
					return response, nil
				}
				if r.URL.Query().Has("versioning") {
					configuration := `<VersioningConfiguration/>`
					if test.versioned {
						configuration = `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`
					}
					return storageResponse(http.StatusOK, configuration), nil
				}
				return storageResponse(http.StatusOK, `<LifecycleConfiguration/>`), nil
			})
			b := StorageBinding{Organization: "org_alpha", Backend: "s3", Endpoint: "https://objects.example.test", Region: "region", Bucket: "private", CredentialReference: "reference"}
			storage, err := NewBindingStorage(t.Context(), b, Credentials{AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret"}, transport)
			if err != nil {
				t.Fatal(err)
			}
			switch test.operation {
			case "versioning":
				_, err = storage.ArchiveVersioning(t.Context())
			case "configure":
				err = storage.ConfigureArchiveReads(t.Context(), []string{"https://app.example.test"})
			case "erase":
				var receipt DeletionReceipt
				receipt, err = storage.(OrganizationEraser).EraseOrganization(t.Context(), "org_alpha")
				if test.oversized && (receipt.Complete || len(receipt.Remaining) == 0) {
					t.Fatal("oversized listing completed deletion")
				}
			case "object":
				var body io.ReadCloser
				body, err = storage.Open(t.Context(), "orgs/org_alpha/blobs/sha256/aa/object")
				if err == nil {
					_, err = io.Copy(io.Discard, body)
					err = errors.Join(err, body.Close())
				}
			}
			if (err != nil) != test.oversized {
				t.Fatalf("oversized=%t error=%v", test.oversized, err)
			}
			if len(bodies) == 0 {
				t.Fatal("response fixture was not read")
			}
			for _, body := range bodies {
				if !body.closed || body.read > limit+1 {
					t.Fatalf("unbounded response: read=%d closed=%t limit=%d", body.read, body.closed, limit)
				}
			}
		})
	}
}

func TestArchiveOrganizationErasure(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                          string
		versioned, partial, foreign, revoked, bounded bool
	}{
		{name: "current objects"},
		{name: "versions and delete markers", versioned: true},
		{name: "partial retry", versioned: true, partial: true},
		{name: "foreign listing", versioned: true, foreign: true},
		{name: "revoked credentials", revoked: true},
		{name: "bounded deletion batches", versioned: true, bounded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			type version struct{ Key, Version string }
			objects := map[version]bool{{"orgs/org_alpha/blobs/sha256/a/blob", "v1"}: true, {"orgs/org_alpha/blobs/sha256/a/blob", "v2"}: true, {"orgs/org_alpha/attachments/item", "marker"}: true, {"orgs/org_beta/blobs/sha256/a/blob", "v3"}: true}
			if test.bounded {
				for index := range 1005 {
					objects[version{"orgs/org_alpha/blobs/sha256/a/" + strconv.Itoa(index), "version"}] = true
				}
			}
			fail := test.partial
			deletes := 0
			transport := storageTransport(func(r *http.Request) (*http.Response, error) {
				if test.revoked {
					return storageResponse(http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>fixture-secret</Message></Error>`), nil
				}
				if r.Method == http.MethodGet && r.URL.Query().Has("versioning") {
					body := `<VersioningConfiguration/>`
					if test.versioned {
						body = `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`
					}
					return storageResponse(http.StatusOK, body), nil
				}
				if r.Method == http.MethodGet {
					if r.URL.Query().Get("prefix") != "orgs/org_alpha/" {
						t.Fatal("wrong deletion prefix")
					}
					var body strings.Builder
					start := `<ListBucketResult><IsTruncated>false</IsTruncated>`
					if test.versioned {
						start = `<ListVersionsResult><IsTruncated>false</IsTruncated>`
					}
					body.WriteString(start)
					for object := range objects {
						if !strings.HasPrefix(object.Key, "orgs/org_alpha/") && !test.foreign {
							continue
						}
						if test.versioned {
							tag := "Version"
							if object.Version == "marker" {
								tag = "DeleteMarker"
							}
							body.WriteString("<" + tag + "><Key>" + object.Key + "</Key><VersionId>" + object.Version + "</VersionId></" + tag + ">")
						} else {
							body.WriteString("<Contents><Key>" + object.Key + "</Key></Contents>")
						}
					}
					if test.versioned {
						body.WriteString("</ListVersionsResult>")
					} else {
						body.WriteString("</ListBucketResult>")
					}
					return storageResponse(http.StatusOK, body.String()), nil
				}
				if r.Method == http.MethodDelete {
					key := strings.TrimPrefix(r.URL.Path, "/private/")
					if !strings.HasPrefix(key, "orgs/org_alpha/") {
						t.Fatal("another tenant deleted")
					}
					deletes++
					if fail && r.URL.Query().Get("versionId") == "v1" {
						return storageResponse(http.StatusForbidden, `<Error><Code>AccessDenied</Code></Error>`), nil
					}
					for object := range objects {
						if object.Key == key && (!test.versioned || object.Version == r.URL.Query().Get("versionId")) {
							delete(objects, object)
						}
					}
					return storageResponse(http.StatusNoContent, ""), nil
				}
				t.Fatalf("unexpected request %s", r.Method)
				return storageResponse(http.StatusInternalServerError, "unexpected request"), nil
			})
			b := StorageBinding{Organization: "org_alpha", Backend: "s3", Endpoint: "https://objects.example.test", Region: "region", Bucket: "private", CredentialReference: "fixture"}
			storage, err := NewBindingStorage(t.Context(), b, Credentials{AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret"}, transport)
			if err != nil {
				t.Fatal(err)
			}
			eraser := storage.(OrganizationEraser)
			receipt, err := eraser.EraseOrganization(t.Context(), "org_alpha")
			if test.partial || test.foreign || test.revoked || test.bounded {
				if err == nil || receipt.Complete || len(receipt.Remaining) == 0 {
					t.Fatalf("failure lost: receipt=%+v error=%v", receipt, err)
				}
				if test.partial {
					if len(receipt.Remaining) != 1 || receipt.Remaining[0].VersionID != "v1" || receipt.Remaining[0].Code != "delete_refused" {
						t.Fatalf("imprecise receipt: %+v", receipt)
					}
					fail = false
					receipt, err = eraser.EraseOrganization(t.Context(), "org_alpha")
				} else if test.bounded {
					if !receipt.Truncated || deletes != 1000 {
						t.Fatalf("unbounded erasure: deletes=%d receipt=%+v", deletes, receipt)
					}
					receipt, err = eraser.EraseOrganization(t.Context(), "org_alpha")
				} else {
					return
				}
			}
			if err != nil || !receipt.Complete || len(objects) != 1 || !objects[version{"orgs/org_beta/blobs/sha256/a/blob", "v3"}] {
				t.Fatalf("erasure incomplete: remaining=%v receipt=%+v error=%v", objects, receipt, err)
			}
			before := deletes
			if receipt, err = eraser.EraseOrganization(t.Context(), "org_alpha"); err != nil || !receipt.Complete || deletes != before {
				t.Fatal("erasure was not idempotent")
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
