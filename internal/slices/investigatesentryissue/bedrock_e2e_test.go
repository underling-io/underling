package investigatesentryissue_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"heinzel/internal/events"
)

// The Bedrock tests run the slice end to end in Bedrock mode, with a fake
// AWS credentials provider in place of the SDK's default chain. The SDK's
// credentials cache reads the real clock, so credentials expire relative
// to time.Now.

// fakeAWS hands out credentials in turn, repeating the last; or err.
type fakeAWS struct {
	mu    sync.Mutex
	sets  []aws.Credentials
	err   error
	calls int
}

func (f *fakeAWS) Retrieve(context.Context) (aws.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return aws.Credentials{}, f.err
	}
	return f.sets[min(f.calls, len(f.sets))-1], nil
}

func (f *fakeAWS) retrieved() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func withAWS(p aws.CredentialsProvider) option {
	return func(s *setup) { s.aws = p }
}

// bedrockMode switches the harness to Bedrock mode, without the two keys.
func bedrockMode(p aws.CredentialsProvider) []option {
	return []option{
		withEnv("ANTHROPIC_API_KEY", ""),
		withEnv("OPENAI_API_KEY", ""),
		withEnv("CLAUDE_CODE_USE_BEDROCK", "1"),
		withEnv("AWS_REGION", "eu-central-1"),
		withEnv("ANTHROPIC_MODEL", "global.anthropic.claude-sonnet-4-5-20250929-v1:0"),
		withEnv("CODEX_BEDROCK_MODEL", "openai.gpt-oss-120b"),
		withEnv("CODEX_BEDROCK_REGION", "eu-west-1"),
		withAWS(p),
	}
}

func sessionCredentials(id string, expires time.Time) aws.Credentials {
	return aws.Credentials{AccessKeyID: id, SecretAccessKey: id + "-secret", SessionToken: id + "-session", CanExpire: true, Expires: expires}
}

// childEnv returns call's env as a map, and its sorted keys.
func childEnv(t *testing.T, call fakeCall) (map[string]string, []string) {
	t.Helper()
	env := map[string]string{}
	var keys []string
	for _, kv := range call.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return env, keys
}

const wantCodexConfig = `model = 'openai.gpt-oss-120b'
model_provider = 'amazon-bedrock'

[model_providers]
[model_providers.amazon-bedrock]
[model_providers.amazon-bedrock.aws]
region = 'eu-west-1'
`

func TestBedrockModeHandsTheChildAWSCredentialsAndACodexConfig(t *testing.T) {
	for _, tt := range []struct {
		name  string
		creds aws.Credentials
		want  []string // the child's env keys besides the fixed ones
	}{
		{"session credentials", sessionCredentials("ASIA1", time.Now().Add(2*time.Hour)), []string{"AWS_SESSION_TOKEN"}},
		{"long-term credentials", aws.Credentials{AccessKeyID: "AKIA1", SecretAccessKey: "AKIA1-secret"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, modeNoop, bedrockMode(&fakeAWS{sets: []aws.Credentials{tt.creds}})...)

			h.handler.Handle(alert(5, 1, now))

			h.assertStream(startedThenOutcome(events.SentryIssueInvestigationNoChangesType), nil)
			calls := fakeCalls(t, h.workDir)
			if len(calls) != 1 {
				t.Fatalf("claude ran %d times, want 1", len(calls))
			}
			env, keys := childEnv(t, calls[0])
			want := append([]string{
				"PATH", "HOME", "CLAUDE_CONFIG_DIR",
				"CLAUDE_CODE_USE_BEDROCK", "AWS_REGION", "ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
				"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
				"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
			}, tt.want...)
			slices.Sort(want)
			if !slices.Equal(keys, want) {
				t.Errorf("child env has %v, want exactly %v", keys, want)
			}
			wantValues := map[string]string{
				"CLAUDE_CODE_USE_BEDROCK":        "1",
				"AWS_REGION":                     "eu-central-1",
				"ANTHROPIC_MODEL":                "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				"ANTHROPIC_SMALL_FAST_MODEL":     "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				"ANTHROPIC_DEFAULT_OPUS_MODEL":   "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				"ANTHROPIC_DEFAULT_SONNET_MODEL": "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				"ANTHROPIC_DEFAULT_FABLE_MODEL":  "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				"AWS_ACCESS_KEY_ID":              tt.creds.AccessKeyID,
				"AWS_SECRET_ACCESS_KEY":          tt.creds.SecretAccessKey,
				"AWS_SESSION_TOKEN":              tt.creds.SessionToken,
			}
			for k, v := range wantValues {
				if env[k] != v {
					t.Errorf("child env %s = %q, want %q", k, env[k], v)
				}
			}
			if calls[0].CodexConfig != wantCodexConfig {
				t.Errorf("codex config =\n%s\nwant\n%s", calls[0].CodexConfig, wantCodexConfig)
			}
			h.assertNoLeftovers()
		})
	}
}

func TestBedrockModeRefetchesCredentialsThatWouldExpireDuringARun(t *testing.T) {
	// The first set, fetched by the first run, outlasts the default 30m
	// timeout by half a second, so by the second run it would expire
	// within it.
	const margin = 500 * time.Millisecond
	var mu sync.Mutex
	var fetched []time.Time
	provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		mu.Lock()
		defer mu.Unlock()
		fetched = append(fetched, time.Now())
		if len(fetched) == 1 {
			return sessionCredentials("ASIA1", fetched[0].Add(30*time.Minute+margin)), nil
		}
		return sessionCredentials("ASIA2", time.Now().Add(2*time.Hour)), nil
	})
	h := newHarness(t, modeNoop, bedrockMode(provider)...)

	h.handler.Handle(issueAlert(5, "1001", 1, now))
	mu.Lock()
	firstExpiresInWindow := fetched[0].Add(margin)
	mu.Unlock()
	time.Sleep(time.Until(firstExpiresInWindow) + 10*time.Millisecond)
	h.handler.Handle(issueAlert(6, "1002", 1, now))

	calls := fakeCalls(t, h.workDir)
	if len(calls) != 2 {
		t.Fatalf("claude ran %d times, want 2; logs:\n%s", len(calls), h.logText())
	}
	var ids []string
	for _, c := range calls {
		env, _ := childEnv(t, c)
		ids = append(ids, env["AWS_ACCESS_KEY_ID"])
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"ASIA1", "ASIA2"}; !slices.Equal(ids, want) || len(fetched) != 2 {
		t.Errorf("runs got credentials %v after %d fetches, want %v after 2", ids, len(fetched), want)
	}
}

func TestBedrockModeFailsARunWhoseRefetchedCredentialsAlsoExpireTooSoon(t *testing.T) {
	// The first set lets the first run start, then falls within the
	// timeout; the refetched set expires within it too.
	const margin = 500 * time.Millisecond
	var mu sync.Mutex
	var fetched []time.Time
	var soon time.Time
	provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		mu.Lock()
		defer mu.Unlock()
		fetched = append(fetched, time.Now())
		if len(fetched) == 1 {
			return sessionCredentials("ASIA1", fetched[0].Add(30*time.Minute+margin)), nil
		}
		soon = time.Now().Add(10 * time.Minute)
		return sessionCredentials("ASIA2", soon), nil
	})
	h := newHarness(t, modeNoop, bedrockMode(provider)...)

	h.handler.Handle(issueAlert(5, "1001", 1, now))
	mu.Lock()
	firstExpiresInWindow := fetched[0].Add(margin)
	mu.Unlock()
	time.Sleep(time.Until(firstExpiresInWindow) + 10*time.Millisecond)
	h.handler.Handle(issueAlert(6, "1002", 1, now))

	var types []string
	var f events.SentryIssueInvestigationFailed
	for _, e := range h.investigation("1002") {
		types = append(types, e.Type)
		if e.Type == events.SentryIssueInvestigationFailedType {
			if err := json.Unmarshal(e.Payload, &f); err != nil {
				t.Fatalf("decode %s: %v", e.Type, err)
			}
		}
	}
	if want := startedThenOutcome(events.SentryIssueInvestigationFailedType); !slices.Equal(types, want) {
		t.Fatalf("second stream holds %v, want %v; logs:\n%s", types, want, h.logText())
	}
	mu.Lock()
	defer mu.Unlock()
	if f.Stage != events.InvestigationFailedClaude || !strings.Contains(f.Reason, soon.UTC().Format(time.RFC3339)) {
		t.Errorf("got %+v, want stage %s naming the expiry %s", f, events.InvestigationFailedClaude, soon.UTC().Format(time.RFC3339))
	}
	if calls := fakeCalls(t, h.workDir); len(calls) != 1 || len(fetched) != 2 {
		t.Errorf("claude ran %d times after %d fetches, want 1 (the first run's) after 2", len(calls), len(fetched))
	}
}

func TestBedrockModeFailsARunWhoseFreshCredentialsExpireTooSoon(t *testing.T) {
	expires := time.Now().Add(10 * time.Minute)
	provider := &fakeAWS{sets: []aws.Credentials{sessionCredentials("ASIA1", expires)}}
	h := newHarness(t, modeNoop, bedrockMode(provider)...)

	h.handler.Handle(alert(5, 1, now))

	assertFailed(t, h, events.InvestigationFailedClaude, "expire")
	var f events.SentryIssueInvestigationFailed
	h.assertStream(startedThenOutcome(events.SentryIssueInvestigationFailedType), &f)
	if !strings.Contains(f.Reason, expires.UTC().Format(time.RFC3339)) || !strings.Contains(f.Reason, "30m0s") {
		t.Errorf("reason %q should name the expiry %s and the timeout", f.Reason, expires.UTC().Format(time.RFC3339))
	}
	if calls := fakeCalls(t, h.workDir); len(calls) != 0 || provider.retrieved() != 1 {
		t.Errorf("claude ran %d times after %d fetches, want 0 after 1", len(calls), provider.retrieved())
	}
	h.assertNoLeftovers()
}

// timeoutError is an error that says it timed out, as SDK and net errors
// do.
type timeoutError struct{}

func (timeoutError) Error() string   { return "dial tcp 169.254.170.2:80: i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestBedrockModeFailsARunWithoutCredentialsAsStageClaude(t *testing.T) {
	for name, err := range map[string]error{
		"an error":        errors.New("no EC2 IMDS role found"),
		"a timeout error": timeoutError{},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, modeNoop, bedrockMode(&fakeAWS{err: err})...)

			h.handler.Handle(alert(5, 1, now))

			assertFailed(t, h, events.InvestigationFailedClaude, err.Error())
			if calls := fakeCalls(t, h.workDir); len(calls) != 0 {
				t.Errorf("claude ran %d times, want 0", len(calls))
			}
			h.assertNoLeftovers()
		})
	}
}

func TestBedrockModeResolvesCredentialsThroughTheDefaultChain(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("AWS_CONFIG_FILE", missing)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", missing)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret")
	t.Setenv("AWS_SESSION_TOKEN", "env-session")
	h := newHarness(t, modeNoop, bedrockMode(nil)...)

	h.handler.Handle(alert(5, 1, now))

	h.assertStream(startedThenOutcome(events.SentryIssueInvestigationNoChangesType), nil)
	calls := fakeCalls(t, h.workDir)
	if len(calls) != 1 {
		t.Fatalf("claude ran %d times, want 1", len(calls))
	}
	env, _ := childEnv(t, calls[0])
	if env["AWS_ACCESS_KEY_ID"] != "AKIAENV" || env["AWS_SECRET_ACCESS_KEY"] != "env-secret" || env["AWS_SESSION_TOKEN"] != "env-session" {
		t.Errorf("child env = %v, want heinzel's env credentials", env)
	}
}
