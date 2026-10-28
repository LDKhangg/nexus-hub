.PHONY: all run build tidy clean test

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
