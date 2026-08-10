package storage

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type s3API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
	HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
}

type loadConfigFunc func(context.Context, ...func(*awsconfig.LoadOptions) error) (aws.Config, error)

// AWSFactory creates SDK-backed S3 stores. loadConfig is an internal
// dependency boundary so configuration-provider failures are passed through
// the same safe error wrapper as service calls.
type AWSFactory struct {
	loadConfig loadConfigFunc
}

func (f AWSFactory) New(ctx context.Context, opts Options) (Store, error) {
	if err := validateFactoryOptions(opts); err != nil {
		return nil, safeS3Error("create client", err)
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(opts.Region),
	}
	if opts.Credentials.Explicit {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				opts.Credentials.AccessKeyID,
				opts.Credentials.SecretAccessKey,
				opts.Credentials.SessionToken,
			),
		))
	}

	loader := f.loadConfig
	if loader == nil {
		loader = awsconfig.LoadDefaultConfig
	}
	cfg, err := loader(ctx, loadOptions...)
	if err != nil {
		return nil, safeS3Error("load configuration", err)
	}
	tracker := newCredentialTracker(cfg.Credentials)
	if tracker != nil {
		cfg.Credentials = tracker
	}
	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		if opts.Endpoint != "" {
			options.BaseEndpoint = aws.String(opts.Endpoint)
		}
		options.UsePathStyle = opts.ForcePathStyle
	})
	clientOptions := client.Options()
	trustedServiceMetadata := clientOptions.BaseEndpoint == nil && cfg.EndpointResolver == nil &&
		cfg.EndpointResolverWithOptions == nil && len(cfg.ServiceOptions) == 0
	metadataFilter := &s3ErrorMetadataPolicy{
		credentials:            tracker,
		trustedServiceMetadata: trustedServiceMetadata,
	}
	uploader := newFileUploader(client)
	uploader.metadataFilter = metadataFilter
	return &s3Store{client: client, uploader: uploader, metadataFilter: metadataFilter}, nil
}

type s3Store struct {
	client         s3API
	uploader       *fileUploader
	metadataFilter s3MetadataFilter
}

func (s *s3Store) UploadFile(ctx context.Context, bucket, key string, source io.ReaderAt, size int64) (UploadResult, error) {
	if s == nil || s.uploader == nil {
		return UploadResult{}, safeS3Error("upload object", errors.New("S3 uploader is not configured"))
	}
	return s.uploader.Upload(ctx, bucket, key, source, size)
}

func (s *s3Store) Probe(ctx context.Context, bucket string) error {
	if s == nil || s.client == nil {
		return safeS3Error("head bucket", errors.New("S3 client is not configured"))
	}
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	return safeS3ErrorWithFilter("head bucket", err, s.metadataFilter)
}

// credentialTracker remembers every successfully retrieved credential set so
// response metadata copied from an untrusted endpoint cannot echo current or
// recently rotated access keys, secret keys, or session tokens. The underlying
// error is still retained by S3Error for programmatic inspection.
type credentialTracker struct {
	provider aws.CredentialsProvider
	mu       sync.RWMutex
	values   map[string]struct{}
}

func newCredentialTracker(provider aws.CredentialsProvider) *credentialTracker {
	if provider == nil {
		return nil
	}
	return &credentialTracker{provider: provider, values: make(map[string]struct{})}
}

func (t *credentialTracker) Retrieve(ctx context.Context) (aws.Credentials, error) {
	credentials, err := t.provider.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, err
	}
	t.mu.Lock()
	for _, value := range []string{credentials.AccessKeyID, credentials.SecretAccessKey, credentials.SessionToken} {
		if value != "" {
			t.values[value] = struct{}{}
		}
	}
	t.mu.Unlock()
	return credentials, nil
}

func (t *credentialTracker) allowsValue(value string) bool {
	if t == nil {
		return true
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for secret := range t.values {
		if strings.Contains(value, secret) {
			return false
		}
	}
	return true
}

// s3ErrorMetadataPolicy treats configured or environment-resolved endpoints as
// untrusted. Their response IDs are never humanized, and only known S3 error
// codes are retained. Standard AWS endpoints may include shape-vetted IDs, but
// credential material is excluded in either case.
type s3ErrorMetadataPolicy struct {
	credentials            *credentialTracker
	trustedServiceMetadata bool
}

func (p *s3ErrorMetadataPolicy) allowsS3Metadata(kind s3MetadataKind, value string) bool {
	if p == nil {
		return false
	}
	if p.credentials != nil && !p.credentials.allowsValue(value) {
		return false
	}
	if p.trustedServiceMetadata {
		return true
	}
	return kind == s3MetadataCode && knownS3ErrorCodes[value]
}

var knownS3ErrorCodes = map[string]bool{
	"AccessDenied":                       true,
	"BadDigest":                          true,
	"BadRequest":                         true,
	"BucketAlreadyExists":                true,
	"BucketAlreadyOwnedByYou":            true,
	"BucketNotEmpty":                     true,
	"EntityTooLarge":                     true,
	"EntityTooSmall":                     true,
	"ExpiredToken":                       true,
	"Forbidden":                          true,
	"IncompleteBody":                     true,
	"InternalError":                      true,
	"InvalidAccessKeyId":                 true,
	"InvalidArgument":                    true,
	"InvalidBucketName":                  true,
	"InvalidDigest":                      true,
	"InvalidPart":                        true,
	"InvalidPartOrder":                   true,
	"InvalidRange":                       true,
	"InvalidRequest":                     true,
	"InvalidSecurity":                    true,
	"InvalidToken":                       true,
	"InvalidURI":                         true,
	"MalformedACLError":                  true,
	"MalformedPOSTRequest":               true,
	"MalformedXML":                       true,
	"MethodNotAllowed":                   true,
	"MissingContentLength":               true,
	"NoSuchBucket":                       true,
	"NoSuchKey":                          true,
	"NoSuchUpload":                       true,
	"NotFound":                           true,
	"NotImplemented":                     true,
	"OperationAborted":                   true,
	"PermanentRedirect":                  true,
	"PreconditionFailed":                 true,
	"RequestExpired":                     true,
	"RequestTimeout":                     true,
	"RequestTimeTooSkewed":               true,
	"ServiceUnavailable":                 true,
	"SignatureDoesNotMatch":              true,
	"SlowDown":                           true,
	"TemporaryRedirect":                  true,
	"TokenRefreshRequired":               true,
	"TooManyBuckets":                     true,
	"UnexpectedContent":                  true,
	"XAmzContentSHA256Mismatch":          true,
	"AuthorizationHeaderMalformed":       true,
	"IllegalLocationConstraintException": true,
}

var factoryRegionPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var factoryHostLabelPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func validateFactoryOptions(opts Options) error {
	if len(opts.Region) > 63 || !factoryRegionPattern.MatchString(opts.Region) {
		return errors.New("invalid S3 region")
	}
	if opts.Endpoint != "" {
		parsed, err := url.ParseRequestURI(opts.Endpoint)
		if err != nil || !validFactoryEndpoint(opts.Endpoint, parsed) {
			return errors.New("invalid S3 endpoint")
		}
	}
	if opts.Credentials.Explicit && (opts.Credentials.AccessKeyID == "" || opts.Credentials.SecretAccessKey == "") {
		return errors.New("incomplete explicit S3 credentials")
	}
	return nil
}

func validFactoryEndpoint(raw string, parsed *url.URL) bool {
	if parsed == nil || !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 || strings.Contains(raw, "#") ||
		parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return false
	}
	path, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil || !utf8.ValidString(path) || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return false
	}
	host := parsed.Hostname()
	if host == "" || !validFactoryHost(host) || strings.HasSuffix(parsed.Host, ":") {
		return false
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return false
		}
	}
	return true
}

func validFactoryHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !factoryHostLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}
