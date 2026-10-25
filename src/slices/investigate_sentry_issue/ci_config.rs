//! Which changed paths are CI configuration, which heinzel won't push.

/// The repo-root files CI services read their pipeline definitions from.
const CI_CONFIG_FILES: [&str; 9] = [
    ".gitlab-ci.yml",
    "Jenkinsfile",
    "azure-pipelines.yml",
    ".travis.yml",
    "bitbucket-pipelines.yml",
    ".drone.yml",
    ".woodpecker.yml",
    "appveyor.yml",
    ".appveyor.yml",
];

/// The repo-root dirs CI services read their pipeline definitions from.
/// The name alone counts too: a file, symlink or submodule there could
/// become, or point at, the dir.
const CI_CONFIG_DIRS: [&str; 6] = [
    ".github",
    ".circleci",
    ".buildkite",
    ".gitlab",
    ".woodpecker",
    ".azure-pipelines",
];

/// How many paths a failure reason lists.
pub(super) const MAX_LISTED_PATHS: usize = 5;

/// Reports whether `path` is CI configuration. A pushed branch that changes
/// it could run a pipeline of Claude's making with the repo's CI secrets.
fn ci_config_path(path: &str) -> bool {
    CI_CONFIG_FILES.contains(&path)
        || CI_CONFIG_DIRS.iter().any(|dir| {
            path.strip_prefix(dir)
                .is_some_and(|rest| rest.is_empty() || rest.starts_with('/'))
        })
}

/// Returns the paths that are CI configuration, in order.
pub(super) fn ci_config_paths(paths: &[String]) -> Vec<&str> {
    paths
        .iter()
        .map(String::as_str)
        .filter(|path| ci_config_path(path))
        .collect()
}

/// Returns at most the first `n` items, noting how many it left out.
pub(super) fn first_n(items: &[&str], n: usize) -> Vec<String> {
    let mut kept: Vec<String> = items.iter().take(n).map(|s| (*s).to_owned()).collect();
    if items.len() > n {
        kept.push(format!("and {} more", items.len() - n));
    }
    kept
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ci_config_path_matches_ci_dirs_and_root_files_only() {
        let cases = [
            (".github/workflows/ci.yml", true),
            (".github/dependabot.yml", true),
            (".github/actions/x/a.yml", true),
            (".gitlab-ci.yml", true),
            (".circleci/config.yml", true),
            (".buildkite/pipeline.yml", true),
            ("Jenkinsfile", true),
            ("azure-pipelines.yml", true),
            (".travis.yml", true),
            ("bitbucket-pipelines.yml", true),
            (".github", true),
            (".circleci", true),
            (".buildkite", true),
            (".gitlab/ci/build.yml", true),
            (".gitlab", true),
            (".drone.yml", true),
            (".woodpecker.yml", true),
            (".woodpecker/build.yml", true),
            ("appveyor.yml", true),
            (".appveyor.yml", true),
            (".azure-pipelines/ci.yml", true),
            (".gitlabx/file", false),
            ("docs/appveyor.yml", false),
            ("src/app.js", false),
            ("docs/.github/notes.md", false),
            (".githubx/file", false),
            ("README.md", false),
            ("scripts/azure-pipelines.md", false),
        ];
        for (path, want) in cases {
            assert_eq!(ci_config_path(path), want, "{path}");
        }
    }

    #[test]
    fn ci_config_paths_keeps_the_order() {
        let paths = ["src/app.js", ".travis.yml", "README.md", "Jenkinsfile"].map(String::from);

        assert_eq!(ci_config_paths(&paths), [".travis.yml", "Jenkinsfile"]);
    }

    #[test]
    fn first_n_notes_what_it_left_out() {
        assert_eq!(first_n(&["a", "b"], 2), ["a", "b"]);
        assert_eq!(first_n(&["a", "b", "c", "d"], 2), ["a", "b", "and 2 more"]);
    }
}
