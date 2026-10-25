//! Path helpers for the read denials: lexical cleaning and containment, as
//! Go's `filepath` does them, and symlink resolution.

use std::path::Path;

/// Returns `path` cleaned lexically, like Go's `filepath.Clean`: no empty or
/// `.` segments, `..` folded where it can be, no trailing slash.
pub(crate) fn clean(path: &str) -> String {
    let absolute = path.starts_with('/');
    let mut segments: Vec<&str> = Vec::new();
    for segment in path.split('/') {
        match segment {
            "" | "." => {}
            ".." => match segments.last() {
                Some(&last) if last != ".." => {
                    segments.pop();
                }
                // `/..` is `/`.
                _ if absolute => {}
                _ => segments.push(".."),
            },
            _ => segments.push(segment),
        }
    }
    let joined = segments.join("/");
    match (absolute, joined.is_empty()) {
        (true, _) => format!("/{joined}"),
        (false, true) => ".".to_owned(),
        (false, false) => joined,
    }
}

/// Returns `path` with symlinks resolved, or just cleaned if it can't be
/// resolved (e.g. it doesn't exist).
pub(crate) fn resolve_path(path: &str) -> String {
    resolved(path).unwrap_or_else(|| clean(path))
}

fn resolved(path: &str) -> Option<String> {
    std::fs::canonicalize(path).ok()?.into_os_string().into_string().ok()
}

/// Returns the non-empty paths, each followed by its symlink-resolved form
/// when that differs, without duplicates. Denying both keeps the child out
/// whichever form it reaches the path by.
pub(crate) fn with_resolved(paths: &[String]) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    let mut add = |p: String| {
        if !p.is_empty() && !out.contains(&p) {
            out.push(p);
        }
    };
    for p in paths.iter().filter(|p| !p.is_empty()) {
        add(p.clone());
        if let Some(r) = resolved(p) {
            add(r);
        }
    }
    out
}

/// Reports whether `path` is `root` or lies under it, compared lexically.
pub(crate) fn within(path: &str, root: &str) -> bool {
    let (path, root) = (clean(path), clean(root));
    if path.starts_with('/') != root.starts_with('/') {
        return false;
    }
    match root.as_str() {
        "/" => true,
        "." => path != ".." && !path.starts_with("../"),
        _ => path == root || path.starts_with(&format!("{root}/")),
    }
}

/// Returns the first of `roots` that `path` lies within.
pub(crate) fn first_containing<'a>(roots: &'a [String], path: &str) -> Option<&'a str> {
    roots
        .iter()
        .find(|root| within(path, root))
        .map(String::as_str)
}

/// Finds `bin` as the child would: as given if it holds a `/`, otherwise in
/// the dirs of `path_var` (a `PATH` value).
pub(crate) fn look_path(bin: &str, path_var: &str) -> Option<String> {
    if bin.contains('/') {
        return Some(bin.to_owned());
    }
    path_var
        .split(':')
        .filter(|dir| !path_var.is_empty() && dir.starts_with('/'))
        .map(|dir| clean(&format!("{dir}/{bin}")))
        .find(|candidate| Path::new(candidate).metadata().is_ok_and(|m| !m.is_dir()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clean_matches_go() {
        let cases = [
            ("/home/heinzel/", "/home/heinzel"),
            ("/a//b/./c", "/a/b/c"),
            ("/a/b/../c", "/a/c"),
            ("/../a", "/a"),
            ("/", "/"),
            ("", "."),
            ("a/..", "."),
            ("../a", "../a"),
            ("a/../../b", "../b"),
        ];
        for (path, want) in cases {
            assert_eq!(clean(path), want, "{path:?}");
        }
    }

    #[test]
    fn within_compares_whole_segments() {
        let cases = [
            ("/a/b", "/a", true),
            ("/a", "/a", true),
            ("/a/", "/a", true),
            ("/ab", "/a", false),
            ("/a", "/a/b", false),
            ("/x", "/", true),
            ("/a/../b", "/a", false),
            ("a", "/a", false),
            ("a/b", ".", true),
            ("../b", ".", false),
        ];
        for (path, root, want) in cases {
            assert_eq!(within(path, root), want, "within({path:?}, {root:?})");
        }
    }

    #[test]
    fn first_containing_returns_the_first_match() {
        let roots = ["/x".to_owned(), "/a".to_owned(), "/a/b".to_owned()];

        assert_eq!(first_containing(&roots, "/a/b/c"), Some("/a"));
        assert_eq!(first_containing(&roots, "/c"), None);
    }

    #[test]
    fn with_resolved_adds_the_resolved_form_once() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("target");
        std::fs::create_dir(&target).unwrap();
        let link = dir.path().join("link");
        std::os::unix::fs::symlink(&target, &link).unwrap();
        let link = link.to_str().unwrap().to_owned();
        let resolved = resolve_path(target.to_str().unwrap());

        let got = with_resolved(&[link.clone(), String::new(), link.clone(), "/no/such".into()]);

        assert_eq!(got, [link, resolved, "/no/such".to_owned()]);
    }

    #[test]
    fn look_path_searches_absolute_path_dirs_for_a_file() {
        let dir = tempfile::tempdir().unwrap();
        let bin_dir = dir.path().join("bin");
        std::fs::create_dir_all(bin_dir.join("claude-dir")).unwrap();
        std::fs::write(bin_dir.join("claude"), "").unwrap();
        let bin_dir = bin_dir.to_str().unwrap();

        assert_eq!(look_path("/opt/claude", ""), Some("/opt/claude".to_owned()));
        assert_eq!(look_path("claude", &format!("relative:{bin_dir}")), Some(format!("{bin_dir}/claude")));
        assert_eq!(look_path("claude-dir", bin_dir), None);
        assert_eq!(look_path("claude", ""), None);
    }
}
