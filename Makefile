BINARY := bin/heinzel-linux-arm64

.PHONY: build-linux-arm64 check system-test clean

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o $(BINARY) ./cmd/heinzel

check:
	go vet ./...
	go test -race ./...

# Replays a real upstream bug fix with the real Claude Code; costs tokens.
# Needs CLAUDE_CODE_OAUTH_TOKEN (claude setup-token) or ANTHROPIC_API_KEY,
# and OPENAI_API_KEY.
system-test:
	go test -tags system -timeout 45m -v -count=1 ./internal/systemtest

clean:
	rm -rf bin
