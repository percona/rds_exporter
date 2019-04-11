#!/usr/bin/env sh
set -e

# Detect project specific configuration
GOLANGCI_LINT_CONFIG="$(pwd)/.golangci.yml"
for cfg in .golangci.yml .golangci.toml .golangci.json; do
    if [ -e "${cfg}" ]; then
        GOLANGCI_LINT_CONFIG=${cfg}
    fi
done

# Configuration
if [ -e go.mod ]; then
     # Use go mod with vendoring
    export GOFLAGS=-mod=vendor
else
    PROJECT_SRC=${GOPATH}/src/${GOPACKAGE}
    # Move go code to the source directory
    mkdir -p "${PROJECT_SRC}"
    cp -r . "${PROJECT_SRC}"
    cd "${PROJECT_SRC}"
fi

echo "Running golanglint-ci with configuration '${GOLANGCI_LINT_CONFIG}'"

ERRS=$(golangci-lint run --new-from-rev=HEAD~ --config="${GOLANGCI_LINT_CONFIG}" 2>&1 || true)
if [ -n "${ERRS}" ]; then
    echo "FAIL"
    echo "${ERRS}"
    echo
    exit 1
fi

echo "PASS"
echo
