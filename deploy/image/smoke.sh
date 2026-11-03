#!/bin/sh
# smoke.sh IMAGE runs the underling image locally with dummy Bedrock config,
# using only container settings Fargate allows (no --privileged, no
# --security-opt, no capabilities beyond SYS_PTRACE), and checks that:
#   1. underling logs "app started" within 30s, with no sandbox warning;
#   2. GET /healthz returns 200;
#   3. the sandbox probe passes.
# Checks 1-2 failing exits 1. A probe failure is a warning (exit 0) unless
# SMOKE_REQUIRE_PROBE=1.
set -u

image=${1:?usage: smoke.sh IMAGE}

# A unique name, so cleanup works even if docker run is killed before it
# returns an id. The EXIT trap removes only a container this run created
# (armed only after checking the name is free), and
# the signal traps turn a signal into an exit, so it runs then too.
name=underling-smoke-$$-$(date +%s)
created=0
trap '[ "$created" -eq 1 ] && docker rm -f "$name" >/dev/null 2>&1' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if docker inspect "$name" >/dev/null 2>&1; then
	echo "FAIL: a container named $name already exists"
	exit 1
fi
created=1
docker run -d --name "$name" --platform linux/arm64 --network none --cap-add SYS_PTRACE \
	-e SENTRY_GITHUB_REPOS=1=acme/app \
	-e GITHUB_TOKEN=dummy \
	-e SENTRY_CLIENT_SECRET=dummy \
	-e AWS_REGION=eu-central-1 \
	-e ANTHROPIC_MODEL=global.anthropic.claude-sonnet-4-5-20250929-v1:0 \
	-e CODEX_BEDROCK_MODEL=openai.gpt-oss-120b \
	-e AWS_ACCESS_KEY_ID=dummy-access-key-id \
	-e AWS_SECRET_ACCESS_KEY=dummy-secret-access-key \
	"$image" >/dev/null || { echo "FAIL: docker run $image"; exit 1; }

failed=0

started=0
i=0
while [ "$i" -lt 30 ]; do
	if docker logs "$name" 2>&1 | grep -q '"msg":"app started"'; then
		started=1
		break
	fi
	[ "$(docker inspect -f '{{.State.Running}}' "$name")" = true ] || break
	sleep 1
	i=$((i + 1))
done
logs=$(docker logs "$name" 2>&1)
if [ "$started" -ne 1 ]; then
	echo "FAIL: app started: not logged within 30s; logs:"
	echo "$logs"
	failed=1
elif echo "$logs" | grep -q "won't run inside the sandbox"; then
	echo "FAIL: app started: startup logged a sandbox warning:"
	echo "$logs" | grep "won't run inside the sandbox"
	failed=1
else
	echo "PASS: app started"
fi

code=$(docker exec "$name" curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/healthz)
if [ "$code" = 200 ]; then
	echo "PASS: /healthz returned 200"
else
	echo "FAIL: /healthz returned ${code:-no response}"
	failed=1
fi

if docker exec "$name" underling-sandbox-probe; then
	echo "PASS: sandbox probe"
else
	echo "FAIL: sandbox probe"
	echo "WARNING: sandbox probe failed under Fargate-allowed container settings; Fargate likely can't run the sandbox (see 016.03/016.05)"
	[ "${SMOKE_REQUIRE_PROBE:-}" = 1 ] && failed=1
fi

exit "$failed"
