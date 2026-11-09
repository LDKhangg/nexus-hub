.PHONY: all run build tidy clean test migration-create generate

all: tidy build

run:
	go run cmd/server/main.go

tidy:
	go mod tidy

build:
	go build -o bin/nexus-server cmd/server/main.go

clean:
	rm -rf bin/

test:
	go test -v ./...

migration-create:
	@test -n "$(name)" || (echo "Usage: make migration-create name=create_hub_threads"; exit 1)
	goose -dir db/migrations -s create "$(name)" sql

generate:
	sqlc generate
