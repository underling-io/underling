//! Every slice directory is wired in, and only those.

#[path = "../build/slice_dirs.rs"]
mod slice_dirs;

use std::fs;
use std::path::Path;

use slice_dirs::slice_dirs;
use tempfile::TempDir;

#[test]
fn every_slice_dir_is_wired_in() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("src/slices");

    let dirs = slice_dirs(&root).unwrap();

    assert_eq!(heinzel::slices::NAMES, dirs);
    assert_eq!(heinzel::slices::FACTORIES.len(), dirs.len());
}

fn tree(paths: &[&str]) -> TempDir {
    let root = TempDir::new().unwrap();
    for path in paths {
        let path = root.path().join(path);
        if path.extension().is_some() {
            fs::create_dir_all(path.parent().unwrap()).unwrap();
            fs::write(path, "").unwrap();
        } else {
            fs::create_dir_all(path).unwrap();
        }
    }
    root
}

#[test]
fn slice_dirs_skips_what_is_not_a_slice() {
    let root = tree(&[
        "record_alert/mod.rs",
        "record_alert/callback.rs",
        "investigate2/mod.rs",
        ".hidden/mod.rs",
        "_old/mod.rs",
        "mod.rs",
        "notes.md",
    ]);

    let got = slice_dirs(root.path()).unwrap();

    assert_eq!(got, ["investigate2", "record_alert"]);
}

#[test]
fn slice_dirs_rejects_a_dir_without_mod_rs() {
    let root = tree(&["good/mod.rs", "flat/flat.rs"]);

    let err = slice_dirs(root.path()).unwrap_err();

    assert!(
        err.contains("flat") && err.contains("no mod.rs"),
        "got {err}"
    );
}

#[test]
fn slice_dirs_rejects_a_name_that_is_no_module_name() {
    for name in ["Record", "record-alert", "1st"] {
        let root = tree(&[&format!("{name}/mod.rs")]);

        let err = slice_dirs(root.path()).unwrap_err();

        assert!(err.contains("snake_case"), "{name}: got {err}");
    }
}

#[test]
fn slice_dirs_rejects_a_rust_keyword() {
    for name in ["type", "mod", "async", "crate"] {
        let root = tree(&[&format!("{name}/mod.rs")]);

        let err = slice_dirs(root.path()).unwrap_err();

        assert!(
            err.contains("keyword") && err.contains(name),
            "{name}: got {err}"
        );
    }
}
