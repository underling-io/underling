BINARY := bin/heinzel-linux-arm64

.PHONY: build-linux-arm64 check clean

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o $(BINARY) ./cmd/heinzel

check:
	go vet ./...
	go test -race ./...

clean:
	rm -rf bin
