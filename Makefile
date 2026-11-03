BINARY := bin/heinzel-linux-arm64

.PHONY: build-linux-arm64 check system-test clean

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o $(BINARY) ./cmd/heinzel

check:
	go vet ./...
	go test -race ./...

# Replays a real upstream bug fix with the real Claude Code; costs tokens.
# Needs CLAUDE_CODE_OAUTH_TOKEN (claude setup-token) or ANTHROPIC_API_KEY,
# and OPENAI_API_KEY. Or, with CLAUDE_CODE_USE_BEDROCK=1, Bedrock mode:
# AWS_REGION, ANTHROPIC_MODEL and CODEX_BEDROCK_MODEL (optionally
# ANTHROPIC_SMALL_FAST_MODEL, CODEX_BEDROCK_REGION) and AWS credentials
# (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_SESSION_TOKEN, AWS_PROFILE,
# or container/web-identity credentials)
# instead of the keys.
system-test:
	go test -tags system -timeout 45m -v -count=1 ./internal/systemtest

clean:
	rm -rf bin
