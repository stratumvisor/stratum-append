.PHONY: build test vet clean help

help:
	@echo 'make build  - build static Linux amd64 and arm64 executables'
	@echo 'make test   - run local Go regression tests'
	@echo 'make vet    - run Go static analysis'
	@echo 'make clean  - remove build outputs'

build:
	./scripts/build.sh

test:
	GOTOOLCHAIN=local go test ./...

vet:
	GOTOOLCHAIN=local go vet ./...

clean:
	rm -rf dist
