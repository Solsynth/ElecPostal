APP := elecpostal
SWAG := swag

.PHONY: build run test tidy swagger

build:
	go build ./...

run:
	go run ./cmd

test:
	go test ./...

tidy:
	go mod tidy

swagger:
	$(SWAG) init -g cmd/main.go -o docs --parseDependency --parseInternal
