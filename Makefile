BINARY := bin/heinzel-linux-arm64

# Container image (docs/event-model.md § Container image).
IMAGE ?= heinzel
# The 12-character HEAD commit, with a -dirty suffix when the tree has
# uncommitted or untracked changes. image-push refuses a -dirty tag unless
# IMAGE_TAG is set explicitly.
IMAGE_TAG ?= $(shell git rev-parse --short=12 HEAD)$(shell [ -z "$$(git status --porcelain)" ] || echo -dirty)
# The ECR repository image-push pushes to, e.g.
# 123456789012.dkr.ecr.eu-central-1.amazonaws.com/heinzel.
ECR_REPOSITORY_URL ?=
# Where Claude Code keeps installed plugins. image-context stages the newest
# Codex and auto-task version dirs from it, the rule the system test uses.
PLUGIN_CACHE ?= $(HOME)/.claude/plugins/cache

# Written by image-context; read by image-build.
-include bin/image/plugin-versions.mk

.PHONY: build-linux-arm64 check system-test postdeploy-test image-context image image-build image-push image-smoke infra-check clean

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

# Drives a deployed heinzel through its public webhook and checks the fix in
# a real GitHub repo (infra/README.md § Post-deploy system test). Costs
# Bedrock tokens, takes up to an hour, and FORCE-PUSHES the target repo's
# default branch, deleting its heinzel/* branches and closing their PRs.
# Needs POSTDEPLOY_BASE_URL (https), SENTRY_CLIENT_SECRET,
# POSTDEPLOY_SENTRY_PROJECT_ID, POSTDEPLOY_GITHUB_REPO (owner/name of a
# throwaway repo) and POSTDEPLOY_GITHUB_TOKEN (never GITHUB_TOKEN);
# optionally POSTDEPLOY_LOG_GROUP (default /ecs/heinzel).
postdeploy-test:
	go test -tags postdeploy -timeout 60m -v -count=1 ./internal/systemtest/postdeploy

# Stages the image's build context in bin/image: the binary, the sandbox
# probe, the newest version dir of each plugin (without .in_use/, which
# holds live pid files) and plugin-versions.mk recording those versions.
image-context: build-linux-arm64
	rm -rf bin/image
	mkdir -p bin/image/plugins
	cp $(BINARY) bin/image/heinzel
	cp deploy/image/heinzel-sandbox-probe bin/image/
	@set -e; : > bin/image/plugin-versions.mk; \
	for p in openai-codex/codex:codex:CODEX auto-task/at:at:AT; do \
		dir=$${p%%:*}; rest=$${p#*:}; name=$${rest%%:*}; var=$${rest#*:}; \
		src="$(PLUGIN_CACHE)/$$dir"; \
		version=$$(ls "$$src" 2>/dev/null | sort -V | tail -1); \
		if [ -z "$$version" ]; then echo "plugin $$name is not installed in $$src" >&2; exit 1; fi; \
		cp -R "$$src/$$version" "bin/image/plugins/$$name"; \
		rm -rf "bin/image/plugins/$$name/.in_use"; \
		echo "$${var}_PLUGIN_VERSION := $$version" >> bin/image/plugin-versions.mk; \
		echo "staged plugin $$name $$version from $$src"; \
	done
	chmod -R a+rX bin/image/plugins

# Needs Docker. Builds $(IMAGE):$(IMAGE_TAG) for linux/arm64. image-build
# runs in a sub-make so it reads the plugin-versions.mk image-context wrote.
image: image-context
	@$(MAKE) --no-print-directory image-build

image-build:
	docker build --platform linux/arm64 -f deploy/image/Dockerfile \
		--build-arg CODEX_PLUGIN_VERSION=$(CODEX_PLUGIN_VERSION) \
		--build-arg AT_PLUGIN_VERSION=$(AT_PLUGIN_VERSION) \
		-t $(IMAGE):$(IMAGE_TAG) bin/image

# Needs Docker and AWS credentials allowed to push to ECR_REPOSITORY_URL.
# Pushes the already built $(IMAGE):$(IMAGE_TAG).
image-push:
	@if [ -z "$(ECR_REPOSITORY_URL)" ]; then \
		echo "usage: make image-push ECR_REPOSITORY_URL=<account>.dkr.ecr.<region>.amazonaws.com/<repo> [IMAGE_TAG=<tag>]" >&2; \
		exit 1; \
	fi
	@case '$(IMAGE_TAG)' in *-dirty) \
		if [ '$(origin IMAGE_TAG)' = file ]; then \
			echo "refusing to push $(IMAGE):$(IMAGE_TAG), built from a tree with uncommitted changes; commit them or set IMAGE_TAG explicitly" >&2; \
			exit 1; \
		fi;; \
	esac
	@set -e; url='$(ECR_REPOSITORY_URL)'; registry=$${url%%/*}; \
	region=$$(echo "$$registry" | sed -n 's/^[0-9]*\.dkr\.ecr\.\([a-z0-9-]*\)\.amazonaws\.com$$/\1/p'); \
	if [ -z "$$region" ]; then echo "can't parse the region from registry host $$registry" >&2; exit 1; fi; \
	aws ecr get-login-password --region "$$region" | docker login --username AWS --password-stdin "$$registry"; \
	docker tag '$(IMAGE):$(IMAGE_TAG)' "$$url:$(IMAGE_TAG)"; \
	docker push "$$url:$(IMAGE_TAG)"; \
	echo "pushed $$url:$(IMAGE_TAG)"

# Needs Docker. Runs the image with dummy Bedrock config under settings
# Fargate allows. A sandbox probe failure is only a warning unless
# SMOKE_REQUIRE_PROBE=1.
image-smoke: image
	deploy/image/smoke.sh $(IMAGE):$(IMAGE_TAG)

# Needs OpenTofu and network to registry.opentofu.org (provider download),
# but no AWS credentials: checks the infra/ code's formatting, validates it,
# and runs its tests against a mocked AWS provider. Not part of check.
infra-check:
	tofu -chdir=infra init -backend=false -input=false -lockfile=readonly
	tofu -chdir=infra fmt -check -recursive
	tofu -chdir=infra validate
	tofu -chdir=infra test

clean:
	rm -rf bin
