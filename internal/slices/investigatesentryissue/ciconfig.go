package investigatesentryissue

import (
	"fmt"
	"slices"
	"strings"
)

// ciConfigFiles and ciConfigDirs are the repo paths that CI services read
// their pipeline definitions from.
var (
	ciConfigFiles = []string{".gitlab-ci.yml", "Jenkinsfile", "azure-pipelines.yml", ".travis.yml", "bitbucket-pipelines.yml"}
	ciConfigDirs  = []string{".github/", ".circleci/", ".buildkite/"}
)

// ciConfigPath reports whether path is CI configuration. A pushed branch
// that changes it could run a pipeline of Claude's making with the repo's
// CI secrets.
func ciConfigPath(path string) bool {
	if slices.Contains(ciConfigFiles, path) {
		return true
	}
	for _, dir := range ciConfigDirs {
		if strings.HasPrefix(path, dir) {
			return true
		}
	}
	return false
}

// ciConfigPaths returns the paths that are CI configuration, in order.
func ciConfigPaths(paths []string) []string {
	return slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return !ciConfigPath(p) })
}

// maxListedPaths caps how many paths a failure reason lists.
const maxListedPaths = 5

// firstN returns at most the first n items, noting how many it left out.
func firstN(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(slices.Clone(items[:n]), fmt.Sprintf("and %d more", len(items)-n))
}
