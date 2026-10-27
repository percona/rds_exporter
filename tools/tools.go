//go:build tools

package tools

import (
	_ "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"
	_ "github.com/reviewdog/reviewdog/cmd/reviewdog"
)
