package enhanced

import (
	"context"
	"errors"
	"fmt"
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

var (
	errIsolationBudget = errors.New("log stream isolation budget exhausted")

	// errRejectedAfterAnswer stands for a ResourceNotFoundException returned for a later page of a
	// request whose first page was answered. The answer proved the streams exist, so the rejection is
	// neither theirs nor the group's, and it must not be counted as a missing stream.
	errRejectedAfterAnswer = errors.New("request rejected after a page was answered")
)

// rejectedAfterAnswer returns the error a request fails with when a page after an answered one is
// rejected. The exception is kept in the text but not wrapped: an error wrapping two is read by
// errorKind as a join named by its most telling leaf, and the exception would make that not_found.
func rejectedAfterAnswer(err error) error {
	return fmt.Errorf("%w: %v", errRejectedAfterAnswer, err)
}

// isResourceNotFound reports whether CloudWatch rejected the request because a log stream or the
// log group does not exist. Which of the two it was is not in the error, so it is decided from what
// the requests of a batch answered, in attributeRejections.
func isResourceNotFound(err error) bool {
	var notFound *types.ResourceNotFoundException

	return errors.As(err, &notFound)
}

// isContextError reports whether the scrape ran out of time or was cancelled, in which case
// further requests would fail too.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// onlyContextErrors reports whether running out of time is all that went wrong. A scrape joins the
// errors of its batches, and errors.Is is satisfied by any one of them, so a batch that was
// throttled before the deadline hit a later one would otherwise pass for a scrape that merely ran
// out of time. The two are not alike: the throttled batch is asked again on the next scrape, and only
// if the window is still where it was.
func onlyContextErrors(err error) bool {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return isContextError(err)
	}

	for _, leaf := range joined.Unwrap() {
		if !onlyContextErrors(leaf) {
			return false
		}
	}

	return true
}

// isThrottling reports whether AWS rejected the request for rate limiting. The SDK has already
// exhausted its own retries before the error is returned to the scraper.
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

// errorKindRank orders the kinds by how much they say about why a scrape failed, most telling last.
// A bisect joins the errors of its halves, and the deadline cutting a later half short says nothing
// about the throttle that failed an earlier one, so the leaf with the most to say names the join.
// Every kind is ranked, including the one only the counter produces: a kind left out ranks below the
// empty string, so a join carrying it would be named by any other leaf, however little that leaf says.
func errorKindRank(kind string) int {
	return slices.Index([]string{
		errorKindContext,
		errorKindOther,
		errorKindNotFound,
		errorKindGroupNotFound,
		errorKindAuth,
		errorKindThrottling,
	}, kind)
}

// errorKind classifies a scrape error for the error counter. The kinds are a closed set to keep the
// metric's label cardinality bounded.
func errorKind(err error) string {
	if err == nil {
		return ""
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		kind := ""

		for _, leaf := range joined.Unwrap() {
			if leafKind := errorKind(leaf); errorKindRank(leafKind) > errorKindRank(kind) {
				kind = leafKind
			}
		}

		return kind
	}

	switch {
	case isContextError(err):
		return errorKindContext
	case isThrottling(err):
		return errorKindThrottling
	case isAuth(err):
		return errorKindAuth
	case errors.Is(err, errRejectedAfterAnswer):
		return errorKindOther
	case isResourceNotFound(err):
		return errorKindNotFound
	default:
		return errorKindOther
	}
}
