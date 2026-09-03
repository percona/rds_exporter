package enhanced

import (
	"context"
	"errors"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/smithy-go"
)

const (
	errorKindContext    = "context"
	errorKindThrottling = "throttling"
	errorKindAuth       = "auth"
	errorKindNotFound   = "not_found"
	// errorKindGroupNotFound is reported for the log group rather than for a stream, and only when a
	// whole batch was rejected without a single request in it being answered.
	errorKindGroupNotFound = "group_not_found"
	errorKindOther         = "other"
)

var errIsolationBudget = errors.New("log stream isolation budget exhausted")

// isResourceNotFound reports whether CloudWatch rejected the request because a log stream or the
// log group does not exist. Which of the two it was is not in the error, so it is decided from what
// the requests of a batch answered, in attributeRejection.
func isResourceNotFound(err error) bool {
	var notFound *types.ResourceNotFoundException

	return errors.As(err, &notFound)
}

// isContextError reports whether the scrape ran out of time or was cancelled, in which case
// further requests would fail too.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isThrottling reports whether AWS rejected the request for rate limiting. The SDK has already
// exhausted its own retries by the time the error reaches us.
func isThrottling(err error) bool {
	throttle := retry.ThrottleErrorCode{Codes: retry.DefaultThrottleErrorCodes}

	return throttle.IsErrorThrottle(err) == aws.TrueTernary
}

// isAuth reports whether AWS refused the credentials, which must never be mistaken for a missing
// log stream.
func isAuth(err error) bool {
	return hasAPIErrorCode(err, "AccessDeniedException", "UnrecognizedClientException", "ExpiredTokenException")
}

func hasAPIErrorCode(err error, codes ...string) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}

	return slices.Contains(codes, apiErr.ErrorCode())
}

// errorKind classifies a scrape error for the error counter. The kinds are a closed set to keep the
// metric's label cardinality bounded.
func errorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case isContextError(err):
		return errorKindContext
	case isThrottling(err):
		return errorKindThrottling
	case isAuth(err):
		return errorKindAuth
	case isResourceNotFound(err):
		return errorKindNotFound
	default:
		return errorKindOther
	}
}
