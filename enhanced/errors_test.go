package enhanced

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
)

func apiError(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code, Fault: 0}
}

func TestErrorKind(t *testing.T) {
	t.Parallel()

	notFound := &types.ResourceNotFoundException{ //nolint:exhaustruct
		Message: nil,
	}

	for _, testCase := range []struct {
		name string
		err  error
		want string
	}{
		{name: "no error", err: nil, want: ""},
		{name: "cancelled", err: context.Canceled, want: errorKindContext},
		{name: "out of time", err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded), want: errorKindContext},
		{name: "throttled", err: apiError("ThrottlingException"), want: errorKindThrottling},
		{name: "request limit reached", err: apiError("RequestLimitExceeded"), want: errorKindThrottling},
		{name: "access denied", err: apiError("AccessDeniedException"), want: errorKindAuth},
		{name: "credentials expired", err: apiError("ExpiredTokenException"), want: errorKindAuth},
		{name: "log stream missing", err: fmt.Errorf("wrapped: %w", notFound), want: errorKindNotFound},
		{name: "isolation budget exhausted", err: errIsolationBudget, want: errorKindOther},
		{name: "unrecognized", err: apiError("InternalFailure"), want: errorKindOther},
		{
			// Only not_found may exclude a log stream, so a refused credential must never reach that kind.
			name: "expired credentials alongside a missing stream",
			err:  errors.Join(apiError("ExpiredTokenException"), notFound),
			want: errorKindAuth,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, testCase.want, errorKind(testCase.err))
		})
	}
}
