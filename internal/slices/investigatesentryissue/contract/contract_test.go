package contract_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"underling/internal/slices/investigatesentryissue/contract"
)

func validEnv() contract.Env {
	return contract.Env{
		Run: contract.Run{
			RunID:   "0123456789abcdef0123456789abcdef",
			Branch:  "underling/sentry-42",
			Timeout: 90 * time.Second,
			Bedrock: contract.Bedrock{
				Region:         "eu-central-1",
				Model:          "eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
				SmallFastModel: "eu.anthropic.claude-haiku-4-5-20251001-v1:0",
				CodexModel:     "openai.gpt-5",
				CodexRegion:    "us-east-1",
			},
		},
		URLs: contract.URLs{
			InputBundle:  "https://bucket.example/runs/r/input/repo.bundle?X-Amz-Signature=in",
			Prompt:       "https://bucket.example/runs/r/input/prompt.txt?X-Amz-Signature=prompt",
			OutputBundle: "https://bucket.example/runs/r/output/repo.bundle?X-Amz-Signature=out",
			Result:       "https://bucket.example/runs/r/output/result.json?X-Amz-Signature=result",
		},
	}
}

func getenvOf(pairs []string) func(string) string {
	env := map[string]string{}
	for _, pair := range pairs {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	return func(name string) string { return env[name] }
}

func TestPairsNamesEveryContractVariable(t *testing.T) {
	want := []string{
		"RUNNER_RUN_ID=0123456789abcdef0123456789abcdef",
		"RUNNER_BRANCH=underling/sentry-42",
		"INVESTIGATION_TIMEOUT=1m30s",
		"RUNNER_INPUT_BUNDLE_URL=https://bucket.example/runs/r/input/repo.bundle?X-Amz-Signature=in",
		"RUNNER_PROMPT_URL=https://bucket.example/runs/r/input/prompt.txt?X-Amz-Signature=prompt",
		"RUNNER_OUTPUT_BUNDLE_URL=https://bucket.example/runs/r/output/repo.bundle?X-Amz-Signature=out",
		"RUNNER_RESULT_URL=https://bucket.example/runs/r/output/result.json?X-Amz-Signature=result",
		"AWS_REGION=eu-central-1",
		"ANTHROPIC_MODEL=eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"ANTHROPIC_SMALL_FAST_MODEL=eu.anthropic.claude-haiku-4-5-20251001-v1:0",
		"CODEX_BEDROCK_MODEL=openai.gpt-5",
		"CODEX_BEDROCK_REGION=us-east-1",
	}

	got := validEnv().Pairs()

	if !slices.Equal(got, want) {
		t.Errorf("Pairs = %q, want %q", got, want)
	}
}

func TestParseEnvReadsBackWhatPairsWrote(t *testing.T) {
	want := validEnv()

	got, err := contract.ParseEnv(getenvOf(want.Pairs()))

	if err != nil || got != want {
		t.Fatalf("ParseEnv = %+v, %v; want %+v", got, err, want)
	}
}

func TestParseEnvReportsEveryInvalidVariable(t *testing.T) {
	const secret = "X-Amz-Signature"
	tests := []struct {
		variable, value, want string
	}{
		{"RUNNER_RUN_ID", "", "RUNNER_RUN_ID: must be set"},
		{"RUNNER_RUN_ID", "a/b", `RUNNER_RUN_ID: "a/b" is not [A-Za-z0-9_-]{1,64}`},
		{"RUNNER_RUN_ID", strings.Repeat("a", 65), "is not [A-Za-z0-9_-]{1,64}"},
		{"RUNNER_BRANCH", "", "RUNNER_BRANCH: must be set"},
		{"INVESTIGATION_TIMEOUT", "", "INVESTIGATION_TIMEOUT: must be set"},
		{"INVESTIGATION_TIMEOUT", "soon", "INVESTIGATION_TIMEOUT: "},
		{"INVESTIGATION_TIMEOUT", "-1s", "INVESTIGATION_TIMEOUT: -1s is not positive"},
		{"RUNNER_INPUT_BUNDLE_URL", "", "RUNNER_INPUT_BUNDLE_URL: must be set"},
		{"RUNNER_PROMPT_URL", "ftp://host/x?" + secret, "RUNNER_PROMPT_URL: is not an absolute http(s) URL"},
		{"RUNNER_OUTPUT_BUNDLE_URL", "/relative?" + secret, "RUNNER_OUTPUT_BUNDLE_URL: is not an absolute http(s) URL"},
		{"RUNNER_RESULT_URL", "https://host/%zz?" + secret, "RUNNER_RESULT_URL: is not an absolute http(s) URL"},
		{"AWS_REGION", "", "AWS_REGION: must be set"},
		{"AWS_REGION", "Frankfurt", `AWS_REGION: "Frankfurt" is not an AWS region such as eu-central-1`},
		{"ANTHROPIC_MODEL", "a model", `ANTHROPIC_MODEL: "a model" is not a Bedrock model id of [A-Za-z0-9._:/-]`},
		{"ANTHROPIC_SMALL_FAST_MODEL", "", "ANTHROPIC_SMALL_FAST_MODEL: must be set"},
		{"CODEX_BEDROCK_MODEL", "", "CODEX_BEDROCK_MODEL: must be set"},
		{"CODEX_BEDROCK_REGION", "", "CODEX_BEDROCK_REGION: must be set"},
	}
	for _, tt := range tests {
		t.Run(tt.variable+"="+tt.value, func(t *testing.T) {
			getenv := getenvOf(validEnv().Pairs())

			_, err := contract.ParseEnv(func(name string) string {
				if name == tt.variable {
					return tt.value
				}
				return getenv(name)
			})

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("err = %v, shows a URL's query", err)
			}
		})
	}

	_, err := contract.ParseEnv(func(string) string { return "" })
	if err == nil || strings.Count(err.Error(), "must be set") != 12 {
		t.Errorf("empty env: err = %v, want 12 missing variables", err)
	}
}

func TestParseResultURL(t *testing.T) {
	got, err := contract.ParseResultURL(getenvOf([]string{"RUNNER_RESULT_URL=http://127.0.0.1:1/r?sig=1"}))
	if err != nil || got != "http://127.0.0.1:1/r?sig=1" {
		t.Errorf("ParseResultURL = %q, %v", got, err)
	}

	_, err = contract.ParseResultURL(func(string) string { return "" })
	if err == nil || err.Error() != "RUNNER_RESULT_URL: must be set" {
		t.Errorf("unset: err = %v", err)
	}
}

func TestNewRunIDIs32HexCharsAndValid(t *testing.T) {
	a, err := contract.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := contract.NewRunID()

	if len(a) != 32 || strings.Trim(a, "0123456789abcdef") != "" || a == b {
		t.Errorf("run ids %q, %q", a, b)
	}
	env := validEnv()
	env.RunID = a
	if _, err := contract.ParseEnv(getenvOf(env.Pairs())); err != nil {
		t.Errorf("a generated run id does not parse: %v", err)
	}
}

func TestObjectKeys(t *testing.T) {
	got := []string{
		contract.ObjectKey("r1", contract.InputBundle),
		contract.ObjectKey("r1", contract.InputPrompt),
		contract.ObjectKey("r1", contract.OutputBundle),
		contract.ObjectKey("r1", contract.OutputResult),
	}
	want := []string{"runs/r1/input/repo.bundle", "runs/r1/input/prompt.txt", "runs/r1/output/repo.bundle", "runs/r1/output/result.json"}
	if !slices.Equal(got, want) {
		t.Errorf("keys = %q, want %q", got, want)
	}
}

func TestEncodeAlwaysWritesAllFourKeys(t *testing.T) {
	tests := []struct {
		result contract.Result
		want   string
	}{
		{contract.Result{Status: contract.StatusOK, Summary: "Fixed it."}, `{"version":1,"status":"ok","summary":"Fixed it.","reason":""}`},
		{contract.Result{Status: contract.StatusOK}, `{"version":1,"status":"ok","summary":"","reason":""}`},
		{contract.Result{Status: contract.StatusFailed, Reason: "exit status 1"}, `{"version":1,"status":"failed","summary":"","reason":"exit status 1"}`},
		{contract.Result{Status: contract.StatusTimeout, Reason: "timed out after 1s"}, `{"version":1,"status":"timeout","summary":"","reason":"timed out after 1s"}`},
	}
	for _, tt := range tests {
		got, err := tt.result.Encode()
		if err != nil || string(got) != tt.want {
			t.Errorf("Encode(%+v) = %s, %v; want %s", tt.result, got, err, tt.want)
		}
	}
}

func TestEncodeRejectsWhatDecodeWould(t *testing.T) {
	for _, result := range []contract.Result{
		{Status: "done"},
		{Status: contract.StatusFailed},
		{Status: contract.StatusTimeout},
	} {
		if _, err := result.Encode(); err == nil {
			t.Errorf("Encode(%+v) succeeded", result)
		}
	}
}

func TestEncodeCutsLongTextAtARuneBoundary(t *testing.T) {
	// 64 KiB is not a multiple of 3, so a cut at exactly the limit would
	// split a rune.
	long := strings.Repeat("€", 30000)

	data, err := contract.Result{Status: contract.StatusFailed, Summary: long, Reason: long}.Encode()
	if err != nil {
		t.Fatal(err)
	}

	var got contract.Result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"summary": got.Summary, "reason": got.Reason} {
		if len(text) != 65535 || !utf8.ValidString(text) || !strings.HasPrefix(long, text) {
			t.Errorf("%s: %d bytes, valid UTF-8 %v", name, len(text), utf8.ValidString(text))
		}
	}
	if len(data) > contract.MaxResultBytes {
		t.Errorf("encoded result is %d bytes, over the limit", len(data))
	}
}

func TestEncodeStaysUnderTheLimitWhenEveryByteIsEscaped(t *testing.T) {
	worst := strings.Repeat("\x01", 70000) // six encoded bytes each

	data, err := contract.Result{Status: contract.StatusFailed, Summary: worst, Reason: worst}.Encode()

	if err != nil || len(data) > contract.MaxResultBytes {
		t.Errorf("encoded %d bytes, %v; limit %d", len(data), err, contract.MaxResultBytes)
	}
}

func TestDecodeResult(t *testing.T) {
	got, err := contract.DecodeResult([]byte(`{"version":1,"status":"ok","summary":"Fixed it.","reason":""}`))
	if want := (contract.Result{Version: 1, Status: contract.StatusOK, Summary: "Fixed it."}); err != nil || got != want {
		t.Errorf("DecodeResult = %+v, %v; want %+v", got, err, want)
	}

	tests := []struct{ name, data, want string }{
		{"not JSON", `<Error>`, "result is not JSON"},
		{"unknown version", `{"version":2,"status":"ok","summary":"","reason":""}`, "result has version 2, want 1"},
		{"no version", `{"status":"ok"}`, "result has version 0, want 1"},
		{"unknown status", `{"version":1,"status":"done","summary":"","reason":""}`, `result has unknown status "done"`},
		{"failed without a reason", `{"version":1,"status":"failed","summary":"","reason":""}`, `result has status "failed" and no reason`},
		{"timeout without a reason", `{"version":1,"status":"timeout","summary":"","reason":""}`, `result has status "timeout" and no reason`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := contract.DecodeResult([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestRedactReplacesEachSecretURLAndItsQuery(t *testing.T) {
	urls := validEnv().URLs
	secrets := contract.Secrets(urls.InputBundle, urls.Result, "", "https://bucket.example/unsigned")
	text := "PUT " + urls.Result + " failed; try ?X-Amz-Signature=in or " + urls.InputBundle + "#top, not " + urls.Prompt

	got := contract.Redact(text, secrets)

	if want := "PUT [redacted] failed; try ?[redacted] or [redacted]#top, not " + urls.Prompt; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactTakesTheLongerSecretFirst(t *testing.T) {
	secrets := contract.Secrets("https://h/a?sig=1", "https://h/a?sig=1&more=2")

	got := contract.Redact("see https://h/a?sig=1&more=2", secrets)

	if got != "see [redacted]" {
		t.Errorf("got %q", got)
	}
}

func TestRedactWithoutSecretsChangesNothing(t *testing.T) {
	if got := contract.Redact("a  b", contract.Secrets("")); got != "a  b" {
		t.Errorf("got %q", got)
	}
}
