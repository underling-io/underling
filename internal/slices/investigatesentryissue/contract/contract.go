// Package contract is what underling and the runner agree on
// (docs/event-model.md § Runner): the runner's environment, the
// names of the files they exchange and the result file.
package contract

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"
	"unicode/utf8"
)

// The runner's environment. Every variable is required.
const (
	RunIDEnvVar           = "RUNNER_RUN_ID"
	BranchEnvVar          = "RUNNER_BRANCH"
	TimeoutEnvVar         = "INVESTIGATION_TIMEOUT"
	InputBundleURLEnvVar  = "RUNNER_INPUT_BUNDLE_URL"
	PromptURLEnvVar       = "RUNNER_PROMPT_URL"
	OutputBundleURLEnvVar = "RUNNER_OUTPUT_BUNDLE_URL"
	ResultURLEnvVar       = "RUNNER_RESULT_URL"
	RegionEnvVar          = "AWS_REGION"
	ModelEnvVar           = "ANTHROPIC_MODEL"
	SmallFastModelEnvVar  = "ANTHROPIC_SMALL_FAST_MODEL"
	CodexModelEnvVar      = "CODEX_BEDROCK_MODEL"
	CodexRegionEnvVar     = "CODEX_BEDROCK_REGION"
)

// The files of a run, by their name under the run's prefix.
const (
	InputBundle  = "input/repo.bundle"
	InputPrompt  = "input/prompt.txt"
	OutputBundle = "output/repo.bundle"
	OutputResult = "output/result.json"
)

// ObjectKey returns the key of a run's file: runs/<runID>/<name>.
func ObjectKey(runID, name string) string { return "runs/" + runID + "/" + name }

// Bedrock is the model and region settings of a run, defaults applied.
type Bedrock struct {
	Region         string // Claude's AWS region
	Model          string // Claude's model or inference-profile id
	SmallFastModel string
	CodexModel     string
	CodexRegion    string
}

// Run is what underling decides about a run, apart from where its files
// are.
type Run struct {
	RunID   string
	Branch  string        // the investigation branch Claude commits to
	Timeout time.Duration // Claude's
	Bedrock Bedrock
}

// URLs are a run's four files as absolute http(s) URLs, presigned in
// production: the runner GETs the first two and PUTs the others.
type URLs struct {
	InputBundle  string
	Prompt       string
	OutputBundle string
	Result       string
}

// Env is the runner's whole contract environment.
type Env struct {
	Run
	URLs
}

// Pairs returns e as NAME=value pairs; ParseEnv reads them back.
func (e Env) Pairs() []string {
	return []string{
		RunIDEnvVar + "=" + e.RunID,
		BranchEnvVar + "=" + e.Branch,
		TimeoutEnvVar + "=" + e.Timeout.String(),
		InputBundleURLEnvVar + "=" + e.URLs.InputBundle,
		PromptURLEnvVar + "=" + e.URLs.Prompt,
		OutputBundleURLEnvVar + "=" + e.URLs.OutputBundle,
		ResultURLEnvVar + "=" + e.URLs.Result,
		RegionEnvVar + "=" + e.Bedrock.Region,
		ModelEnvVar + "=" + e.Bedrock.Model,
		SmallFastModelEnvVar + "=" + e.Bedrock.SmallFastModel,
		CodexModelEnvVar + "=" + e.Bedrock.CodexModel,
		CodexRegionEnvVar + "=" + e.Bedrock.CodexRegion,
	}
}

var (
	runID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// awsRegion is an AWS region's shape, such as eu-central-1; it ends up
	// in a hostname.
	awsRegion = regexp.MustCompile(`^[a-z]+(-[a-z]+)+-[0-9]+$`)
	// modelID is a Bedrock model or inference-profile id; it ends up in
	// TOML and a request path.
	modelID = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)
)

var errUnset = errors.New("must be set")

// CheckRegion reports why value is not an AWS region.
func CheckRegion(value string) error {
	if !awsRegion.MatchString(value) {
		return fmt.Errorf("%q is not an AWS region such as eu-central-1", value)
	}
	return nil
}

// CheckModel reports why value is not a Bedrock model id.
func CheckModel(value string) error {
	if !modelID.MatchString(value) {
		return fmt.Errorf("%q is not a Bedrock model id of [A-Za-z0-9._:/-]", value)
	}
	return nil
}

func checkRunID(value string) error {
	if !runID.MatchString(value) {
		return fmt.Errorf("%q is not [A-Za-z0-9_-]{1,64}", value)
	}
	return nil
}

// checkURL reports why value is not an absolute http(s) URL. The error
// never shows value: a presigned URL's query is a credential.
func checkURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("is not an absolute http(s) URL")
	}
	return nil
}

func parseTimeout(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s is not positive", value)
	}
	return d, nil
}

// ParseEnv reads the runner's environment through getenv. It reports
// every missing or invalid variable, each by name.
func ParseEnv(getenv func(string) string) (Env, error) {
	var errs []error
	// read returns variable's value after check accepted it.
	read := func(variable string, check func(string) error) string {
		value := getenv(variable)
		err := errUnset
		if value != "" {
			err = check(value)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", variable, err))
		}
		return value
	}
	any := func(string) error { return nil }

	var e Env
	e.RunID = read(RunIDEnvVar, checkRunID)
	e.Branch = read(BranchEnvVar, any)
	read(TimeoutEnvVar, func(value string) (err error) {
		e.Timeout, err = parseTimeout(value)
		return err
	})
	e.URLs.InputBundle = read(InputBundleURLEnvVar, checkURL)
	e.URLs.Prompt = read(PromptURLEnvVar, checkURL)
	e.URLs.OutputBundle = read(OutputBundleURLEnvVar, checkURL)
	e.URLs.Result = read(ResultURLEnvVar, checkURL)
	e.Bedrock.Region = read(RegionEnvVar, CheckRegion)
	e.Bedrock.Model = read(ModelEnvVar, CheckModel)
	e.Bedrock.SmallFastModel = read(SmallFastModelEnvVar, CheckModel)
	e.Bedrock.CodexModel = read(CodexModelEnvVar, CheckModel)
	e.Bedrock.CodexRegion = read(CodexRegionEnvVar, CheckRegion)
	if len(errs) > 0 {
		return Env{}, errors.Join(errs...)
	}
	return e, nil
}

// ParseResultURL reads only the result's URL, so that a runner whose
// environment is otherwise broken can still say so in a result.
func ParseResultURL(getenv func(string) string) (string, error) {
	value := getenv(ResultURLEnvVar)
	err := errUnset
	if value != "" {
		err = checkURL(value)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", ResultURLEnvVar, err)
	}
	return value, nil
}

// NewRunID returns a random run id of 32 hex characters.
func NewRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate a run id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Version is the result file's format version.
const Version = 1

// MaxResultBytes is the largest encoded result underling reads.
const MaxResultBytes = 1 << 20

// maxTextBytes is where Encode cuts a result's summary and reason. Both
// at their worst, every byte a six-byte JSON escape, still fit in
// MaxResultBytes.
const maxTextBytes = 64 << 10

// Status is how a run ended.
type Status string

const (
	StatusOK      Status = "ok"      // Claude finished; the output bundle is uploaded
	StatusFailed  Status = "failed"  // anything else went wrong
	StatusTimeout Status = "timeout" // Claude ran into the run's timeout
)

// Result is the result file. Failed and timeout results carry a reason.
type Result struct {
	Version int    `json:"version"`
	Status  Status `json:"status"`
	Summary string `json:"summary"` // Claude's
	Reason  string `json:"reason"`
}

func (r Result) check() error {
	switch r.Status {
	case StatusOK:
	case StatusFailed, StatusTimeout:
		if r.Reason == "" {
			return fmt.Errorf("result has status %q and no reason", r.Status)
		}
	default:
		return fmt.Errorf("result has unknown status %q", r.Status)
	}
	return nil
}

// Encode returns r as the result file, at the current Version and with
// the summary and the reason cut to 64 KiB each.
func (r Result) Encode() ([]byte, error) {
	r.Version = Version
	r.Summary = cut(r.Summary, maxTextBytes)
	r.Reason = cut(r.Reason, maxTextBytes)
	if err := r.check(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// cut returns s's longest prefix of at most limit bytes that ends at a
// rune boundary.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// DecodeResult parses a result file and rejects one of another version,
// with an unknown status, or failed without a reason.
func DecodeResult(data []byte) (Result, error) {
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return Result{}, fmt.Errorf("result is not JSON: %s", err.Error())
	}
	if r.Version != Version {
		return Result{}, fmt.Errorf("result has version %d, want %d", r.Version, Version)
	}
	if err := r.check(); err != nil {
		return Result{}, err
	}
	return r, nil
}
