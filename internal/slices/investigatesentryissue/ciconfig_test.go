package investigatesentryissue

import (
	"slices"
	"testing"
)

func TestCIConfigPath(t *testing.T) {
	tests := map[string]bool{
		".github/workflows/ci.yml":   true,
		".github/dependabot.yml":     true,
		".github/actions/x/a.yml":    true,
		".gitlab-ci.yml":             true,
		".circleci/config.yml":       true,
		".buildkite/pipeline.yml":    true,
		"Jenkinsfile":                true,
		"azure-pipelines.yml":        true,
		".travis.yml":                true,
		"bitbucket-pipelines.yml":    true,
		"src/app.js":                 false,
		"docs/.github/notes.md":      false,
		".githubx/file":              false,
		"README.md":                  false,
		"scripts/azure-pipelines.md": false,
	}
	for path, want := range tests {
		if got := ciConfigPath(path); got != want {
			t.Errorf("ciConfigPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCIConfigPathsLeavesItsInputAlone(t *testing.T) {
	paths := []string{"src/app.js", ".travis.yml", "README.md"}

	got := ciConfigPaths(paths)

	if !slices.Equal(got, []string{".travis.yml"}) || !slices.Equal(paths, []string{"src/app.js", ".travis.yml", "README.md"}) {
		t.Fatalf("got %q from %q", got, paths)
	}
}
