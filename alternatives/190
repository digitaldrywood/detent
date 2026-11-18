package attachment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/digitaldrywood/detent/internal/artifact"
)

type StorageBinding struct {
	ID                  string `json:"id"`
	Organization        string `json:"organization_id"`
	Revision            int64  `json:"revision"`
	Backend             string `json:"backend"`
	Endpoint            string `json:"endpoint"`
	Region              string `json:"region"`
	Bucket              string `json:"bucket"`
	Prefix              string `json:"prefix"`
	CredentialReference string `json:"-"`
	Versioned           *bool  `json:"versioned,omitempty"`
}

func (StorageBinding) LogValue() slog.Value { return slog.StringValue("archive storage binding") }

type Credentials struct {
	AccessKeyID     string `json:"-"`
	SecretAccessKey string `json:"-"`
}

func (Credentials) LogValue() slog.Value { return slog.StringValue("archive credentials") }
func (Credentials) String() string       { return "[archive credentials]" }

func CredentialsFromEnvironment(organization, reference string, lookup func(string) string) (Credentials, error) {
	if !validOrganization(organization) || !validOrganization(reference) {
		return Credentials{}, ErrInvalid
	}
	digest := sha256.Sum256([]byte(organization + "\n" + reference))
	prefix := "DETENT_ARCHIVE_" + strings.ToUpper(hex.EncodeToString(digest[:]))
	secret := Credentials{AccessKeyID: lookup(prefix + "_ACCESS_KEY_ID"), SecretAccessKey: lookup(prefix + "_SECRET_ACCESS_KEY")}
	if secret.AccessKeyID == "" || secret.SecretAccessKey == "" {
		return Credentials{}, artifact.ErrStorage
	}
	return secret, nil
}

func (b StorageBinding) Validate() error {
	if !validOrganization(b.Organization) || b.Backend != "s3" && b.Backend != "hosted" || !validOrganization(b.Region) || b.Bucket == "" || len(b.Bucket) > 255 || strings.ContainsAny(b.Bucket, "/\\?#%\r\n\t ") || !validOrganization(b.CredentialReference) || len(b.Prefix) > 512 {
		return ErrInvalid
	}
	if b.Prefix != "" {
		if !strings.HasSuffix(b.Prefix, "/") || strings.HasPrefix(b.Prefix, "/") {
			return ErrInvalid
		}
		for _, part := range strings.Split(strings.TrimSuffix(b.Prefix, "/"), "/") {
			if !validOrganization(part) {
				return ErrInvalid
			}
		}
	}
	u, err := url.Parse(b.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(u.Host, "\"';\r\n\t ") {
		return ErrInvalid
	}
	return nil
}

func publicArchiveAddress(address netip.Addr) bool {
	address = address.Unmap()
	return address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback() && !address.IsLinkLocalUnicast() && !netip.MustParsePrefix("100.64.0.0/10").Contains(address) && !netip.MustParsePrefix("192.0.0.0/24").Contains(address) && !netip.MustParsePrefix("198.18.0.0/15").Contains(address) && !netip.MustParsePrefix("64:ff9b::/96").Contains(address) && !netip.MustParsePrefix("2002::/16").Contains(address)
}

func archiveAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") || strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".localhost") {
		return nil, ErrInvalid
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, artifact.ErrStorage
	}
	for _, address := range addresses {
		if !publicArchiveAddress(address) {
			return nil, ErrInvalid
		}
	}
	return addresses, nil
}

func ValidateArchiveEndpoint(ctx context.Context, endpoint string) error {
	b := StorageBinding{Organization: "org_validation", Backend: "s3", Endpoint: endpoint, Region: "region", Bucket: "bucket", CredentialReference: "reference"}
	if b.Validate() != nil {
		return ErrInvalid
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ErrInvalid
	}
	_, err = archiveAddresses(ctx, u.Hostname())
	return err
}

func archiveTransport(endpoint string) (*http.Transport, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, ErrInvalid
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, artifact.ErrStorage
	}
	transport := base.Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, u.Hostname()) {
			return nil, ErrInvalid
		}
		addresses, err := archiveAddresses(ctx, host)
		if err != nil {
			return nil, err
		}
		dialer := net.Dialer{Timeout: 30 * time.Second}
		var failures []error
		for _, address := range addresses {
			connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
			if err == nil {
				return connection, nil
			}
			failures = append(failures, err)
		}
		return nil, errors.Join(failures...)
	}
	return transport, nil
}

type archiveResponseTransport struct {
	next   http.RoundTripper
	bucket string
}

func (t archiveResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	limit := int64(maxArchiveControlResponseBytes)
	if request.Method == http.MethodGet && response.StatusCode == http.StatusOK && request.URL.Path != "/"+t.bucket && request.URL.Path != "/"+t.bucket+"/" {
		limit = MaxBytes
	}
	data, err := readArchiveResponse(response, limit)
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}

func NewBindingStorage(ctx context.Context, b StorageBinding, secret Credentials, transport http.RoundTripper) (ArchiveStorage, error) {
	if b.Validate() != nil || secret.AccessKeyID == "" || secret.SecretAccessKey == "" {
		return nil, ErrInvalid
	}
	if transport == nil {
		if err := ValidateArchiveEndpoint(ctx, b.Endpoint); err != nil {
			return nil, err
		}
		guarded, err := archiveTransport(b.Endpoint)
		if err != nil {
			return nil, err
		}
		transport = guarded
	}
	client := &http.Client{Transport: archiveResponseTransport{next: transport, bucket: b.Bucket}, Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	store := &spaces{http: client, prefix: b.Prefix, config: Config{Endpoint: b.Endpoint, Region: b.Region, Bucket: b.Bucket}}
	store.client = s3.NewFromConfig(aws.Config{Region: b.Region, Credentials: credentials.NewStaticCredentialsProvider(secret.AccessKeyID, secret.SecretAccessKey, ""), HTTPClient: client, RetryMaxAttempts: 2}, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(b.Endpoint)
		options.UsePathStyle = true
		options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return store, nil
}
