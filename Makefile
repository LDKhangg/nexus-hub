.PHONY: all run build tidy clean test-ci check docs-check migration-create generate

all: tidy build

run:
	go run ./cmd/server

tidy:
	go mod tidy

# Both binaries: the service and the admin CLI (nh).
build:
	go build -o bin/nexus-server ./cmd/server
	go build -o bin/nh ./cmd/setup-notices

clean:
	rm -rf bin/

# The compile checks every role may run (see AGENTS.md).
check:
	test -z "$$(gofmt -l cmd internal)"
	go vet ./...
	go build ./...
	goose -dir db/migrations validate

# Every .md path named in the guides must exist (CI runs this too).
docs-check:
	scripts/check-doc-paths.sh

# CI only: the suite runs in GitHub Actions, not on a dev machine (AGENTS.md, CI).
test-ci:
	@test -n "$$CI" || (echo "test-ci runs in CI only; push the branch and read the run"; exit 1)
	go test -race ./...

migration-create:
	@test -n "$(name)" || (echo "Usage: make migration-create name=create_hub_threads"; exit 1)
	goose -dir db/migrations -s create "$(name)" sql

generate:
	sqlc generate
