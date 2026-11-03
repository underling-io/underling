// Package postdeploy is a system test against a deployed underling: it posts
// a signed Sentry alert for go-version's real panic to the public webhook
// and checks, in a real GitHub repo, that underling opened a draft PR that
// fixes it. Before that it force-pushes the named throwaway repo's default
// branch, deletes its underling/* branches and closes their PRs.
//
// The test is behind the postdeploy build tag, run by make postdeploy-test
// (see infra/README.md § Post-deploy system test). It costs Bedrock tokens
// and takes up to an hour, so neither make check nor go test ./... runs
// it; they run only the unit tests of its pure checks.
package postdeploy
