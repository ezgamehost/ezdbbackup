package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func TestAWSFactoryPreservesEndpointPathPrefixPathStyleAndCredentialSelection(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "default-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "default-secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "default-session-token")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	tests := []struct {
		name        string
		credentials Credentials
		wantAccess  string
		wantSecret  string
		wantToken   string
	}{
		{
			name:       "default credential chain",
			wantAccess: "default-access-key",
			wantSecret: "default-secret-key",
			wantToken:  "default-session-token",
		},
		{
			name: "explicit credentials",
			credentials: Credentials{
				AccessKeyID:     "explicit-access-key",
				SecretAccessKey: "explicit-secret-key",
				SessionToken:    "explicit-session-token",
				Explicit:        true,
			},
			wantAccess: "explicit-access-key",
			wantSecret: "explicit-secret-key",
			wantToken:  "explicit-session-token",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := (AWSFactory{}).New(context.Background(), Options{
				Region:         "us-east-1",
				Endpoint:       "https://objects.example.test/s3/base",
				ForcePathStyle: true,
				Credentials:    test.credentials,
			})
			if err != nil {
				t.Fatalf("AWSFactory.New() error = %v", err)
			}

			client, ok := store.(*s3Store).client.(*s3.Client)
			if !ok {
				t.Fatalf("AWSFactory.New() client type = %T, want *s3.Client", store.(*s3Store).client)
			}
			clientOptions := client.Options()
			if got := aws.ToString(clientOptions.BaseEndpoint); got != "https://objects.example.test/s3/base" {
				t.Errorf("BaseEndpoint = %q, want preserved path prefix", got)
			}
			if !clientOptions.UsePathStyle {
				t.Error("UsePathStyle = false, want true")
			}

			gotCredentials, err := clientOptions.Credentials.Retrieve(context.Background())
			if err != nil {
				t.Fatalf("Retrieve() credentials error = %v", err)
			}
			if gotCredentials.AccessKeyID != test.wantAccess ||
				gotCredentials.SecretAccessKey != test.wantSecret ||
				gotCredentials.SessionToken != test.wantToken {
				t.Error("credential provider did not return the expected credential source")
			}
		})
	}
}

// This fails if an S3 endpoint can reflect authorization/session credentials,
// request URLs, raw messages, body text, or controls into human-facing errors.
func TestS3ErrorBoundarySanitizesEchoingEndpointForDefaultAndExplicitCredentials(t *testing.T) {
	tests := []struct {
		name        string
		credentials Credentials
		access      string
		secret      string
		token       string
	}{
		{
			name:   "default chain session credentials",
			access: "overlap",
			secret: "overlap-secret",
			token:  "overlap-secret-token",
		},
		{
			name:   "explicit session credentials",
			access: "explicit-overlap",
			secret: "explicit-overlap-secret",
			token:  "explicit-overlap-secret-token",
			credentials: Credentials{
				AccessKeyID:     "explicit-overlap",
				SecretAccessKey: "explicit-overlap-secret",
				SessionToken:    "explicit-overlap-secret-token",
				Explicit:        true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", test.access)
			t.Setenv("AWS_SECRET_ACCESS_KEY", test.secret)
			t.Setenv("AWS_SESSION_TOKEN", test.token)
			t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

			var requestPath string
			var reflectedSignature string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requestPath = request.URL.EscapedPath()
				authorization := request.Header.Get("Authorization")
				sessionToken := request.Header.Get("X-Amz-Security-Token")
				reflectedSignature = authorization
				if index := strings.LastIndex(authorization, "Signature="); index >= 0 {
					reflectedSignature = authorization[index+len("Signature="):]
				}
				writer.Header().Set("Content-Type", "application/xml")
				if request.Method == http.MethodPut {
					// Neither value is a literal configured credential, but both are
					// still untrusted response metadata copied from auth/query/body.
					writer.Header().Set("X-Amz-Request-Id", reflectedSignature)
					writer.Header().Set("X-Amz-Id-2", "QUERYSECRET")
					writer.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprintf(writer, "<Error><Code>BODYSECRET</Code><Message>echo %s %s %s?credential=%s</Message></Error>", authorization, sessionToken, request.URL.String(), test.secret)
					return
				}
				// These identifiers have a legal AWS identifier shape, but an
				// untrusted endpoint copied live credential material into them.
				writer.Header().Set("X-Amz-Request-Id", sessionToken)
				writer.Header().Set("X-Amz-Id-2", "host-"+test.secret)
				writer.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprintf(writer, "<Error><Code>AccessDenied</Code><Message>echo %s %s body-control&#10;next %s?credential=%s</Message></Error>", authorization, sessionToken, request.URL.String(), test.secret)
			}))
			defer server.Close()

			store, err := (AWSFactory{}).New(context.Background(), Options{
				Region:         "us-east-1",
				Endpoint:       server.URL + "/s3/base",
				ForcePathStyle: true,
				Credentials:    test.credentials,
			})
			if err != nil {
				t.Fatalf("AWSFactory.New() error = %v", err)
			}
			err = store.Probe(context.Background(), "backups")
			if err == nil {
				t.Fatal("Probe() error = nil")
			}
			text := err.Error()
			for _, forbidden := range []string{test.access, test.secret, test.token, "Authorization", "credential=", server.URL, "body-control", "next"} {
				if forbidden != "" && strings.Contains(text, forbidden) {
					t.Fatalf("Probe() error exposed %q: %q", forbidden, text)
				}
			}
			for _, required := range []string{"head bucket", "code=Forbidden", "status=403"} {
				if !strings.Contains(text, required) {
					t.Fatalf("Probe() error = %q, want safe field %q", text, required)
				}
			}
			if strings.Contains(text, "request_id=") || strings.Contains(text, "host_id=") {
				t.Fatalf("Probe() trusted credential-shaped endpoint metadata: %q", text)
			}
			if requestPath != "/s3/base/backups" {
				t.Fatalf("request path = %q, want endpoint path prefix and path-style bucket", requestPath)
			}
			var apiErr smithy.APIError
			if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "Forbidden" {
				t.Fatalf("errors.As(APIError) = %v, want preserved original API error", apiErr)
			}

			_, err = store.UploadFile(context.Background(), "backups", "object.sql.gz", bytes.NewReader([]byte("x")), 1)
			if err == nil {
				t.Fatal("UploadFile() error = nil")
			}
			for _, forbidden := range []string{"BODYSECRET", "QUERYSECRET", reflectedSignature, test.access, test.secret, test.token, server.URL, "credential="} {
				if forbidden != "" && strings.Contains(err.Error(), forbidden) {
					t.Fatalf("UploadFile() exposed untrusted metadata %q: %q", forbidden, err)
				}
			}
			if got := err.Error(); !strings.Contains(got, "put object") || !strings.Contains(got, "status=400") {
				t.Fatalf("UploadFile() error = %q, want bounded operation/status", got)
			}
			apiErr = nil
			if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "BODYSECRET" {
				t.Fatalf("errors.As(APIError) = %v, want original endpoint error preserved only in chain", apiErr)
			}
		})
	}
}

func TestSafeS3ErrorIncludesSafeNonCredentialMetadata(t *testing.T) {
	original := &maliciousS3Error{
		code:      "AccessDenied",
		message:   "raw endpoint message",
		requestID: "REQ123safe",
		hostID:    "HOST456safe",
		status:    http.StatusForbidden,
	}
	err := safeS3ErrorWithFilter("put object", original, &s3ErrorMetadataPolicy{trustedServiceMetadata: true})
	for _, required := range []string{"code=AccessDenied", "status=403", "request_id=REQ123safe", "host_id=HOST456safe"} {
		if !strings.Contains(err.Error(), required) {
			t.Fatalf("safeS3Error() = %q, want %q", err, required)
		}
	}
}

func TestS3ErrorBoundaryTracksRotatingSessionCredentials(t *testing.T) {
	expired := time.Now().Add(-time.Second)
	sets := []aws.Credentials{
		{AccessKeyID: "rotating-access-one", SecretAccessKey: "rotating-secret-one", SessionToken: "rotating-token-one", CanExpire: true, Expires: expired},
		{AccessKeyID: "rotating-access-two", SecretAccessKey: "rotating-secret-two", SessionToken: "rotating-token-two", CanExpire: true, Expires: expired},
	}
	provider := &rotatingCredentialsProvider{sets: sets}
	cachedProvider := aws.NewCredentialsCache(provider)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token := request.Header.Get("X-Amz-Security-Token")
		writer.Header().Set("X-Amz-Request-Id", token)
		writer.Header().Set("X-Amz-Id-2", "host-"+token)
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	factory := AWSFactory{loadConfig: func(context.Context, ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		return aws.Config{
			Region:      "us-east-1",
			Credentials: cachedProvider,
			HTTPClient:  server.Client(),
		}, nil
	}}
	store, err := factory.New(context.Background(), Options{
		Region: "us-east-1", Endpoint: server.URL, ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range sets {
		err := store.Probe(context.Background(), "backups")
		if err == nil {
			t.Fatalf("Probe() attempt %d error = nil", attempt+1)
		}
		for _, credentials := range sets {
			for _, forbidden := range []string{credentials.AccessKeyID, credentials.SecretAccessKey, credentials.SessionToken} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("Probe() attempt %d exposed rotating credential %q: %q", attempt+1, forbidden, err)
				}
			}
		}
	}
	if got := provider.calls.Load(); got < int32(len(sets)) {
		t.Fatalf("underlying rotating provider calls = %d, want cache refresh for each expired set", got)
	}
}

// This fails if unsafe API/request identifiers are emitted merely because
// they implement the SDK metadata interfaces.
func TestSafeS3ErrorOmitsUnsafeMetadataAndPreservesOriginal(t *testing.T) {
	original := &maliciousS3Error{
		code:      "Unsafe\nCode",
		message:   "raw endpoint message SECRET",
		requestID: "request\nid",
		hostID:    strings.Repeat("H", 300),
		status:    599,
	}
	err := safeS3Error("upload part", original)
	if got := err.Error(); got != "S3 upload part failed (status=599)" {
		t.Fatalf("safeS3Error() = %q, want bounded safe metadata only", got)
	}
	if !errors.Is(err, original) {
		t.Fatal("safeS3Error() did not unwrap to original")
	}
	var got *maliciousS3Error
	if !errors.As(err, &got) || got != original {
		t.Fatalf("errors.As() = %p, want original %p", got, original)
	}
}

// This fails if AWS configuration loading errors bypass the same safe human
// boundary used for service operations.
func TestAWSFactorySanitizesConfigurationLoadFailure(t *testing.T) {
	original := &maliciousS3Error{
		code:      "CONFIGSECRET",
		message:   "credential process echoed CONFIG_SECRET and https://metadata.invalid/?token=CONFIG_SECRET",
		requestID: "CONFIGREQUEST",
		hostID:    "CONFIGHOST",
		status:    http.StatusTeapot,
	}
	factory := AWSFactory{loadConfig: func(context.Context, ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		return aws.Config{}, original
	}}
	_, err := factory.New(context.Background(), Options{Region: "us-east-1"})
	if !errors.Is(err, original) {
		t.Fatalf("AWSFactory.New() error = %v, want preserved load cause", err)
	}
	for _, forbidden := range []string{"CONFIG_SECRET", "CONFIGSECRET", "CONFIGREQUEST", "CONFIGHOST", "metadata.invalid"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("AWSFactory.New() exposed configuration metadata %q: %q", forbidden, err)
		}
	}
	if !strings.Contains(err.Error(), "load configuration") || !strings.Contains(err.Error(), "status=418") {
		t.Fatalf("AWSFactory.New() exposed configuration error: %q", err)
	}
}

func TestAWSFactoryRejectsUnsafeDirectOptionsWithoutEchoingThem(t *testing.T) {
	const marker = "DIRECT_OPTION_SECRET"
	_, err := (AWSFactory{}).New(context.Background(), Options{
		Region:   "us-east-1\n" + marker,
		Endpoint: "https://user:" + marker + "@example.test/?token=" + marker,
	})
	if err == nil {
		t.Fatal("AWSFactory.New() error = nil for unsafe direct options")
	}
	if strings.Contains(err.Error(), marker) || strings.ContainsAny(err.Error(), "\r\n\t") {
		t.Fatalf("AWSFactory.New() exposed unsafe direct option: %q", err)
	}
}

type maliciousS3Error struct {
	code      string
	message   string
	requestID string
	hostID    string
	status    int
}

func (e *maliciousS3Error) Error() string                 { return e.message }
func (e *maliciousS3Error) ErrorCode() string             { return e.code }
func (e *maliciousS3Error) ErrorMessage() string          { return e.message }
func (e *maliciousS3Error) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }
func (e *maliciousS3Error) ServiceRequestID() string      { return e.requestID }
func (e *maliciousS3Error) ServiceHostID() string         { return e.hostID }
func (e *maliciousS3Error) HTTPStatusCode() int           { return e.status }

var _ smithy.APIError = (*maliciousS3Error)(nil)
var _ s3.ResponseError = (*maliciousS3Error)(nil)

type rotatingCredentialsProvider struct {
	sets  []aws.Credentials
	calls atomic.Int32
}

func (p *rotatingCredentialsProvider) Retrieve(context.Context) (aws.Credentials, error) {
	index := int(p.calls.Add(1)) - 1
	if index >= len(p.sets) {
		index = len(p.sets) - 1
	}
	return p.sets[index], nil
}
