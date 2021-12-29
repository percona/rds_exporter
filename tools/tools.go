// rds_exporter

//go:build tools
// +build tools

package tools

import (
	_ "github.com/AlekSi/gocoverutil"
	_ "github.com/golangci/golangci-lint/cmd/golangci-lint"
	_ "github.com/reviewdog/reviewdog/cmd/reviewdog"
)
