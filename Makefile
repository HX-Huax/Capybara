.PHONY: build test fmt vet
build:
	mkdir -p bin
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -o bin/capy ./cmd/capy
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -o bin/capybarad ./cmd/capybarad
test:
	go test ./...
fmt:
	gofmt -w cmd internal
vet:
	go vet ./...
