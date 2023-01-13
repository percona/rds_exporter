# Copyright 2017 The Prometheus Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

GO    := go
pkgs   = $(shell $(GO) list ./...)

PREFIX                  ?= $(shell pwd)
BIN_DIR                 ?= $(shell pwd)
DOCKER_IMAGE_NAME       ?= $(shell basename $(shell pwd))
DOCKER_IMAGE_TAG        ?= $(subst /,-,$(shell git rev-parse --abbrev-ref HEAD))

all: format build test

GO_BUILD_LDFLAGS = -X github.com/prometheus/common/version.Version=$(shell cat VERSION) -X github.com/prometheus/common/version.Revision=$(shell git rev-parse HEAD) -X github.com/prometheus/common/version.Branch=$(shell git describe --always --contains --all) -X github.com/prometheus/common/version.BuildUser= -X github.com/prometheus/common/version.BuildDate=$(shell date +%FT%T%z) -s -w

export PMM_RELEASE_PATH?=.

style:
	@echo ">> checking code style"
	@! gofmt -d $(shell find . -path ./vendor -prune -o -name '*.go' -print) | grep '^'

test:
	@echo ">> running tests"
	@$(GO) test $(pkgs)

test-race:
	@echo ">> running tests"
	@$(GO) test -v -race $(pkgs)

format:
	@echo ">> formatting code"
	@$(GO) fmt $(pkgs)

vet:
	@echo ">> vetting code"
	@$(GO) vet $(pkgs)

docker:
	@echo ">> building docker image $(DOCKER_IMAGE_NAME):$(DOCKER_IMAGE_TAG)"
	@docker build -t "$(DOCKER_IMAGE_NAME):$(DOCKER_IMAGE_TAG)" .

check:
	bin/golangci-lint run -c=.golangci.yml --out-format=line-number

codecov: gocoverutil
	@bin/gocoverutil -coverprofile=coverage.txt test $(pkgs)
	@curl -s https://codecov.io/bash | bash -s - -X fix

gocoverutil:
	@$(GO) build -modfile=tools/go.mod -o bin/gocoverutil github.com/AlekSi/gocoverutil

release:
	go build -ldflags="$(GO_BUILD_LDFLAGS)" -o $(PMM_RELEASE_PATH)/rds_exporter
