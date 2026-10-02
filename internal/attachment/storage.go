package attachment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/conversation"
)

type Config struct {
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"-"`
	SecretAccessKey string `yaml:"-"`
}

type Storage interface {
	Put(context.Context, string, io.ReadSeeker, int64, string, string) error
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
	Walk(context.Context, string, func(Object) error) error
}

type Object struct {
	Key       string
	CreatedAt time.Time
}

type spaces struct {
	client *s3.Client
	http   *http.Client
	config Config
}

func NewStorage(ctx context.Context, cfg Config, transport http.RoundTripper) (Storage, error) {
	if !artifact.ValidOrigin(cfg.Endpoint) || cfg.Region == "" || cfg.Bucket == "" || strings.ContainsAny(cfg.Bucket, "/\\?#") || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("attachments require an endpoint, region, bucket and entry credentials")
	}
	client := &http.Client{Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if transport != nil {
		client.Transport = transport
	}
	store := &spaces{http: client, config: cfg}
	store.client = s3.NewFromConfig(aws.Config{Region: cfg.Region, Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""), HTTPClient: client, RetryMaxAttempts: 2}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	if err := store.verify(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

func Key(organization, id string) (string, error) {
	if !validOrganization(organization) || conversation.ValidateAttachmentID(id) != nil {
		return "", ErrInvalid
	}
	return "orgs/" + organization + "/attachments/" + id, nil
}

func OrganizationPrefix(organization string) (string, error) {
	if !validOrganization(organization) {
		return "", ErrInvalid
	}
	return "orgs/" + organization + "/", nil
}

func validOrganization(value string) bool {
	return value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) == -1
}

func (s *spaces) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, media, digest string) error {
	checksum, err := hex.DecodeString(digest)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(key), Body: body, ContentLength: aws.Int64(size), ContentType: aws.String(media), IfNoneMatch: aws.String("*"), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(checksum))})
	return err
}

func (s *spaces) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	return result.Body, nil
}

func (s *spaces) Delete(ctx context.Context, key string) error {
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(key)})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && (api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchKey") {
			return nil
		}
		return err
	}
	input := &s3.DeleteObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(key)}
	if version := aws.ToString(head.VersionId); version != "" {
		input.VersionId = aws.String(version)
	}
	_, err = s.client.DeleteObject(ctx, input)
	return err
}

func (s *spaces) Walk(ctx context.Context, prefix string, visit func(Object) error) error {
	if !strings.HasPrefix(prefix, "orgs/") || !strings.HasSuffix(prefix, "/") {
		return ErrInvalid
	}
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.config.Bucket), Prefix: aws.String(prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if !strings.HasPrefix(key, prefix) {
				return ErrInvalid
			}
			if object.LastModified == nil {
				return ErrInvalid
			}
			if err := visit(Object{Key: key, CreatedAt: *object.LastModified}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *spaces) verify(ctx context.Context) (resultErr error) {
	key := "probe/" + conversation.NewAttachmentID()
	data := []byte("detent-private-attachments-probe")
	if err := s.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "text/plain", artifact.Digest(data)); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		resultErr = errors.Join(resultErr, s.Delete(cleanup, key))
	}()
	body, err := s.Open(ctx, key)
	if err != nil {
		return err
	}
	read, err := io.ReadAll(io.LimitReader(body, int64(len(data))+1))
	if err = errors.Join(err, body.Close()); err != nil {
		return err
	}
	if !bytes.Equal(data, read) {
		return errors.New("attachment probe integrity mismatch")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.config.Endpoint+"/"+s.config.Bucket+"/"+key, nil)
	if err != nil {
		return err
	}
	response, err := s.http.Do(request)
	if err != nil {
		return err
	}
	if err := response.Body.Close(); err != nil {
		return err
	}
	if response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusNotFound {
		return errors.New("attachments bucket must refuse anonymous reads")
	}
	return nil
}
