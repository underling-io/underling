package investigatesentryissue

import (
	"fmt"
	"slices"
	"strings"
)

// The repo paths CI services read pipelines from. A pushed branch changing
// them could run a pipeline of Claude's making with the repo's CI secrets.
var (
	ciConfigFiles = []string{".gitlab-ci.yml", "Jenkinsfile", "azure-pipelines.yml", ".travis.yml", "bitbucket-pipelines.yml"}
	ciConfigDirs  = []string{".github/", ".circleci/", ".buildkite/"}
)

// maxListedPaths caps how many paths a failure reason lists.
const maxListedPaths = 5

func ciConfigPath(path string) bool {
	return slices.Contains(ciConfigFiles, path) ||
		slices.ContainsFunc(ciConfigDirs, func(dir string) bool { return strings.HasPrefix(path, dir) })
}

// ciConfigPaths returns the paths that are CI configuration, in order.
func ciConfigPaths(paths []string) []string {
	var ci []string
	for _, p := range paths {
		if ciConfigPath(p) {
			ci = append(ci, p)
		}
	}
	return ci
}

// firstN returns at most the first n items, noting how many it left out.
func firstN(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(slices.Clone(items[:n]), fmt.Sprintf("and %d more", len(items)-n))
}
