# Gatewai build tooling.
# On Windows without make: run the same commands directly
# (e.g. `go run ./cmd/gatewai -config configs/gatewai.example.yaml`).

.PHONY: build test bench lint run docker clean

build:
	go build -o bin/gatewai ./cmd/gatewai

test:
	go test -race -coverprofile=coverage.out ./...

bench:
	go test -bench=. -benchmem ./...

lint:
	golangci-lint run

run:
	go run ./cmd/gatewai -config configs/gatewai.yaml

docker:
	docker build -t gatewai .

clean:
	rm -rf bin coverage.out
