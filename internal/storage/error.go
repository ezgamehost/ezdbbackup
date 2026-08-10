package storage

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

var safeOperationPattern = regexp.MustCompile(`^[a-z][a-z0-9 ]{0,63}$`)
var safeCodePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:#-]{0,127}$`)
var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+=-]{0,255}$`)

// S3Error is the human-safe boundary for AWS configuration and service
// failures. Error deliberately never formats Cause; Unwrap retains it for
// errors.Is/errors.As and SDK-specific programmatic handling.
type S3Error struct {
	Operation string
	Code      string
	Status    int
	RequestID string
	HostID    string
	Cause     error
}

func (e *S3Error) Error() string {
	operation := e.Operation
	if !safeOperationPattern.MatchString(operation) {
		operation = "operation"
	}
	text := "S3 " + operation + " failed"
	fields := make([]string, 0, 4)
	if safeCodePattern.MatchString(e.Code) {
		fields = append(fields, "code="+e.Code)
	}
	if e.Status >= 100 && e.Status <= 599 {
		fields = append(fields, fmt.Sprintf("status=%d", e.Status))
	}
	if safeIDPattern.MatchString(e.RequestID) {
		fields = append(fields, "request_id="+e.RequestID)
	}
	if safeIDPattern.MatchString(e.HostID) {
		fields = append(fields, "host_id="+e.HostID)
	}
	if len(fields) != 0 {
		text += " (" + strings.Join(fields, ", ") + ")"
	}
	return text
}

func (e *S3Error) Unwrap() error { return e.Cause }

func safeS3Error(operation string, err error) error {
	return safeS3ErrorWithFilter(operation, err, conservativeS3MetadataFilter{})
}

type conservativeS3MetadataFilter struct{}

func (conservativeS3MetadataFilter) allowsS3Metadata(kind s3MetadataKind, value string) bool {
	return kind == s3MetadataCode && knownS3ErrorCodes[value]
}

type s3MetadataFilter interface {
	allowsS3Metadata(s3MetadataKind, string) bool
}

type s3MetadataKind uint8

const (
	s3MetadataCode s3MetadataKind = iota
	s3MetadataResponseID
)

func safeS3ErrorWithFilter(operation string, err error, filter s3MetadataFilter) error {
	if err == nil {
		return nil
	}
	wrapped := &S3Error{Operation: operation, Cause: err}
	var apiError smithy.APIError
	if errors.As(err, &apiError) && safeMetadata(s3MetadataCode, apiError.ErrorCode(), safeCodePattern, filter) {
		wrapped.Code = apiError.ErrorCode()
	}
	var responseError s3.ResponseError
	if errors.As(err, &responseError) {
		if value := responseError.ServiceRequestID(); safeMetadata(s3MetadataResponseID, value, safeIDPattern, filter) {
			wrapped.RequestID = value
		}
		if value := responseError.ServiceHostID(); safeMetadata(s3MetadataResponseID, value, safeIDPattern, filter) {
			wrapped.HostID = value
		}
	}
	var statusError interface{ HTTPStatusCode() int }
	if errors.As(err, &statusError) {
		status := statusError.HTTPStatusCode()
		if status >= 100 && status <= 599 {
			wrapped.Status = status
		}
	}
	return wrapped
}

func safeMetadata(kind s3MetadataKind, value string, pattern *regexp.Regexp, filter s3MetadataFilter) bool {
	return pattern.MatchString(value) && (filter == nil || filter.allowsS3Metadata(kind, value))
}
