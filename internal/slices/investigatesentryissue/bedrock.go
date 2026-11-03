package investigatesentryissue

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/pelletier/go-toml/v2"

	"underling/internal/slices/investigatesentryissue/claude"
)

// retrieveTimeout bounds how long a run waits for AWS credentials.
const retrieveTimeout = time.Minute

// newCredentialsCache returns the AWS credentials every Bedrock run draws
// from: provider, or the SDK's default chain (env, profile, ECS task role)
// for nil. The cache refetches credentials that expire within timeout, so
// a run never starts on credentials that could lapse before it ends.
func newCredentialsCache(ctx context.Context, provider aws.CredentialsProvider, region string, timeout time.Duration) (aws.CredentialsProvider, error) {
	options := func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = timeout
		o.ExpiryWindowJitterFrac = 0
	}
	if provider != nil {
		return aws.NewCredentialsCache(provider, options), nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region), awsconfig.WithCredentialsCacheOptions(options))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return cfg.Credentials, nil
}

// runCredentials returns credentials from cache that outlast timeout.
//
// Errors are flattened to text: an SDK or net error that reports Timeout
// would otherwise pass for Claude timing out.
func runCredentials(ctx context.Context, cache aws.CredentialsProvider, timeout time.Duration) (claude.AWSCredentials, error) {
	ctx, cancel := context.WithTimeout(ctx, retrieveTimeout)
	defer cancel()
	creds, err := cache.Retrieve(ctx)
	if err != nil {
		return claude.AWSCredentials{}, fmt.Errorf("get AWS credentials: %s", err.Error())
	}
	// The cache moved Expires earlier by timeout and refetches anything
	// past it, so expired credentials here are fresh ones that expire too
	// soon.
	if creds.CanExpire && creds.Expired() {
		return claude.AWSCredentials{}, fmt.Errorf("fresh AWS credentials expire at %s, within the %v investigation timeout",
			creds.Expires.Add(timeout).UTC().Format(time.RFC3339), timeout)
	}
	return claude.AWSCredentials{AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey, SessionToken: creds.SessionToken}, nil
}

// codexConfig is the Codex config.toml underling writes for a Bedrock run.
type codexConfig struct {
	Model          string                   `toml:"model"`
	ModelProvider  string                   `toml:"model_provider"`
	ModelProviders map[string]codexProvider `toml:"model_providers"`
}

type codexProvider struct {
	AWS struct {
		Region string `toml:"region"`
	} `toml:"aws"`
}

// codexProviderID is Codex's built-in provider for Bedrock's Mantle
// endpoint.
const codexProviderID = "amazon-bedrock"

// writeCodexConfig writes home/.codex/config.toml so Codex uses Bedrock.
// The region goes in the file because the child's AWS_REGION is Claude's,
// and Codex reads the file's first. Write errors are flattened to text, as
// in runCredentials: a PathError that reports Timeout would otherwise pass
// for Claude timing out.
func writeCodexConfig(home string, b *bedrockConfig) error {
	var provider codexProvider
	provider.AWS.Region = b.codexRegion
	data, err := toml.Marshal(codexConfig{
		Model:          b.codexModel,
		ModelProvider:  codexProviderID,
		ModelProviders: map[string]codexProvider{codexProviderID: provider},
	})
	if err != nil {
		return fmt.Errorf("encode codex config: %w", err)
	}
	dir := filepath.Join(home, ".codex")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("write codex config: %s", err.Error())
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), data, 0o600); err != nil {
		return fmt.Errorf("write codex config: %s", err.Error())
	}
	return nil
}
