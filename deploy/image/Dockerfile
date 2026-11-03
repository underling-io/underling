# heinzel in Bedrock mode with its host prerequisites (docs/event-model.md
# § Container image). Built by `make image` from the context `make
# image-context` stages in bin/image; this file is the one place versions
# are pinned.
# The multi-arch index digest of node:22.23.4-bookworm-slim, from
# `docker buildx imagetools inspect node:22.23.4-bookworm-slim`.
FROM node:22.23.4-bookworm-slim@sha256:4b150b48b4116e2a22f2c39cc27025fbf4f153e2078fd3b14d159fdb5c7279da

ARG CLAUDE_VERSION=2.1.317
# linux-arm64 checksum from
# https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}/manifest.json
ARG CLAUDE_SHA256=563a0d60ad0c223d9415768210997acb0f7134ea51dc3c238e88bc2e18ddeefa
ARG CODEX_VERSION=0.165.0
# The repo's go.mod toolchain; checksum from https://go.dev/dl/?mode=json
ARG GO_VERSION=1.27.3
ARG GO_SHA256=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
# Debian bookworm's bubblewrap. The sandbox probe's result holds for this
# version, so the build fails if apt installs another.
ARG BWRAP_VERSION=0.8.0

RUN apt-get update \
	&& apt-get install -y --no-install-recommends bubblewrap ca-certificates curl git socat \
	&& rm -rf /var/lib/apt/lists/*

# The native binary, not the install script, which installs into HOME.
RUN curl -fsSL -o /tmp/claude "https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}/linux-arm64/claude" \
	&& echo "${CLAUDE_SHA256}  /tmp/claude" | sha256sum -c - \
	&& install -m 0755 /tmp/claude /usr/local/bin/claude \
	&& rm /tmp/claude \
	&& HOME="$(mktemp -d)" claude --version | grep -q "^${CLAUDE_VERSION} " \
	&& rm -rf /tmp/tmp.*

RUN npm install -g "@openai/codex@${CODEX_VERSION}" \
	&& npm cache clean --force \
	&& test "$(HOME="$(mktemp -d)" codex --version)" = "codex-cli ${CODEX_VERSION}" \
	&& rm -rf /tmp/tmp.*

# The child's env is an exact allowlist, so ENV GOTOOLCHAIN never reaches
# its `go test`; go.env is what keeps the toolchain from downloading another.
RUN curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-arm64.tar.gz" \
	&& echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum -c - \
	&& tar -C /usr/local -xzf /tmp/go.tar.gz \
	&& rm /tmp/go.tar.gz \
	&& sed -i 's/^GOTOOLCHAIN=auto$/GOTOOLCHAIN=local/' /usr/local/go/go.env \
	&& test "$(env -i PATH=/usr/local/go/bin:/usr/bin:/bin HOME=/tmp/x go env GOTOOLCHAIN)" = local \
	&& test "$(env -i PATH=/usr/local/go/bin:/usr/bin:/bin HOME=/tmp/x go env GOVERSION)" = "go${GO_VERSION}" \
	&& rm -rf /tmp/x

RUN test "$(bwrap --version)" = "bubblewrap ${BWRAP_VERSION}" && socat -V >/dev/null

RUN groupadd -g 10001 heinzel \
	&& useradd -u 10001 -g 10001 -m -d /home/heinzel -s /bin/sh heinzel \
	&& install -d -o root -g root -m 0755 /var/lib/heinzel \
	&& install -d -o heinzel -g heinzel -m 0700 /var/lib/heinzel/data /var/lib/heinzel/work

COPY --chmod=0755 heinzel heinzel-sandbox-probe /usr/local/bin/

# Required, no default: make image-context records them from the staged
# plugin dirs.
ARG CODEX_PLUGIN_VERSION
ARG AT_PLUGIN_VERSION
RUN test -n "${CODEX_PLUGIN_VERSION}" && test -n "${AT_PLUGIN_VERSION}" \
	|| { echo "CODEX_PLUGIN_VERSION and AT_PLUGIN_VERSION must be set" >&2; exit 1; }
# make image-context stages them world-readable; COPY keeps those modes.
COPY plugins/ /opt/heinzel/plugins/

LABEL heinzel.claude="${CLAUDE_VERSION}" \
	heinzel.codex="${CODEX_VERSION}" \
	heinzel.go="${GO_VERSION}" \
	heinzel.bwrap="${BWRAP_VERSION}" \
	heinzel.plugins.codex="${CODEX_PLUGIN_VERSION}" \
	heinzel.plugins.at="${AT_PLUGIN_VERSION}"

ENV HOME=/home/heinzel \
	PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
	INVESTIGATION_WORK_DIR=/var/lib/heinzel/work \
	INVESTIGATION_DENY_READ=/var/lib/heinzel/data \
	CLAUDE_PLUGIN_DIRS=/opt/heinzel/plugins/codex,/opt/heinzel/plugins/at \
	CLAUDE_CODE_USE_BEDROCK=1 \
	GOTOOLCHAIN=local

USER heinzel
WORKDIR /home/heinzel
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=15s --retries=3 \
	CMD curl -fsS http://127.0.0.1:8080/healthz
CMD ["heinzel", "-addr", ":8080", "-data-dir", "/var/lib/heinzel/data"]
