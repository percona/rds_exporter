all: test

build:
	go build .

test:
#	go test -v  ./...

.PHONY: all test
