//! A record of every entry in a directory tree, taken without following
//! symlinks, so heinzel can tell whether anything in its mirror changed.

use std::collections::BTreeMap;
use std::fs;
use std::io;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};

use sha2::{Digest, Sha256};

/// Files up to this size are recorded by their content; bigger ones (pack
/// files) by size, mtime and inode.
const HASHED_MAX: u64 = 1 << 20;

/// What one entry in the tree is.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) enum Entry {
    Dir { mode: u32 },
    File { mode: u32, content: Content },
    Symlink { target: PathBuf },
    Other { mode: u32 },
}

/// How a file's content is recorded.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) enum Content {
    Sha256([u8; 32]),
    /// `ctime` can't be set back without root, so a same-size rewrite
    /// that restores the mtime still shows.
    Stat {
        size: u64,
        mtime_ns: i128,
        ctime_ns: i128,
        dev: u64,
        ino: u64,
    },
}

/// Every entry under a root, by path relative to it (`""` is the root).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub(super) struct Manifest(BTreeMap<String, Entry>);

impl Manifest {
    /// Records the tree at `root`.
    pub(super) fn take(root: &Path) -> io::Result<Manifest> {
        let mut entries = BTreeMap::new();
        record(root, root, &mut entries)?;
        Ok(Manifest(entries))
    }

    /// The first path, in path order, that differs between the two.
    pub(super) fn first_difference(&self, other: &Manifest) -> Option<String> {
        let mut paths: Vec<&String> = self.0.keys().chain(other.0.keys()).collect();
        paths.sort();
        paths
            .into_iter()
            .find(|path| self.0.get(*path) != other.0.get(*path))
            .cloned()
    }

    /// The dirs in the tree with the modes they had, root first.
    pub(super) fn dirs(&self) -> impl Iterator<Item = (&str, u32)> {
        self.0.iter().filter_map(|(path, entry)| match entry {
            Entry::Dir { mode } => Some((path.as_str(), *mode)),
            _ => None,
        })
    }

    /// The first symlink in the tree, if any.
    pub(super) fn first_symlink(&self) -> Option<&str> {
        self.0
            .iter()
            .find(|(_, entry)| matches!(entry, Entry::Symlink { .. }))
            .map(|(path, _)| path.as_str())
    }
}

fn record(root: &Path, path: &Path, entries: &mut BTreeMap<String, Entry>) -> io::Result<()> {
    let meta = path.symlink_metadata()?;
    let mode = meta.permissions().mode() & 0o7777;
    let file_type = meta.file_type();
    let entry = if file_type.is_symlink() {
        Entry::Symlink {
            target: fs::read_link(path)?,
        }
    } else if file_type.is_dir() {
        Entry::Dir { mode }
    } else if file_type.is_file() {
        let content = if meta.len() <= HASHED_MAX {
            Content::Sha256(Sha256::digest(fs::read(path)?).into())
        } else {
            Content::Stat {
                size: meta.len(),
                mtime_ns: i128::from(meta.mtime()) * 1_000_000_000 + i128::from(meta.mtime_nsec()),
                ctime_ns: i128::from(meta.ctime()) * 1_000_000_000 + i128::from(meta.ctime_nsec()),
                dev: meta.dev(),
                ino: meta.ino(),
            }
        };
        Entry::File { mode, content }
    } else {
        Entry::Other { mode }
    };
    let rel = path
        .strip_prefix(root)
        .map_err(io::Error::other)?
        .to_string_lossy()
        .into_owned();
    let is_dir = matches!(entry, Entry::Dir { .. });
    entries.insert(rel, entry);
    if is_dir {
        for child in fs::read_dir(path)? {
            record(root, &child?.path(), entries)?;
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tree() -> tempfile::TempDir {
        let root = tempfile::tempdir().unwrap();
        fs::create_dir_all(root.path().join("refs/heads")).unwrap();
        fs::write(root.path().join("config"), "[core]\n").unwrap();
        fs::write(root.path().join("refs/heads/main"), "abc\n").unwrap();
        root
    }

    #[test]
    fn an_untouched_tree_matches() {
        let root = tree();

        let before = Manifest::take(root.path()).unwrap();

        assert_eq!(before.first_difference(&Manifest::take(root.path()).unwrap()), None);
    }

    #[test]
    fn every_kind_of_change_shows() {
        type Change = fn(&Path);
        let cases: [(&str, Change, &str); 6] = [
            ("content", |r| fs::write(r.join("config"), "[core]\nx\n").unwrap(), "config"),
            ("new file", |r| fs::write(r.join("commondir"), "/x\n").unwrap(), "commondir"),
            ("removed file", |r| fs::remove_file(r.join("refs/heads/main")).unwrap(), "refs/heads/main"),
            ("new dir", |r| fs::create_dir(r.join("refs/replace")).unwrap(), "refs/replace"),
            (
                "mode",
                |r| fs::set_permissions(r.join("refs"), fs::Permissions::from_mode(0o777)).unwrap(),
                "refs",
            ),
            ("symlink", |r| std::os::unix::fs::symlink("/etc", r.join("objects")).unwrap(), "objects"),
        ];
        for (name, change, path) in cases {
            let root = tree();
            let before = Manifest::take(root.path()).unwrap();

            change(root.path());

            let after = Manifest::take(root.path()).unwrap();
            assert_eq!(before.first_difference(&after).as_deref(), Some(path), "{name}");
        }
    }

    #[test]
    fn a_big_file_rewritten_in_place_with_its_mtime_restored_shows() {
        use std::io::{Seek, SeekFrom, Write};
        let root = tree();
        let pack = root.path().join("pack");
        fs::write(&pack, vec![b'a'; 2 << 20]).unwrap();
        let mtime = fs::metadata(&pack).unwrap().modified().unwrap();
        let before = Manifest::take(root.path()).unwrap();
        // Let the clock move on, so a new ctime differs.
        std::thread::sleep(std::time::Duration::from_millis(20));

        let mut file = fs::OpenOptions::new().write(true).open(&pack).unwrap();
        file.seek(SeekFrom::Start(1000)).unwrap();
        file.write_all(b"evil").unwrap();
        file.set_modified(mtime).unwrap();
        drop(file);

        let meta = fs::metadata(&pack).unwrap();
        assert_eq!((meta.len(), meta.modified().unwrap()), (2 << 20, mtime), "the forgery holds");
        let after = Manifest::take(root.path()).unwrap();
        assert_eq!(before.first_difference(&after).as_deref(), Some("pack"));
    }

    #[test]
    fn a_symlink_is_recorded_not_followed() {
        let root = tree();
        let outside = tempfile::tempdir().unwrap();
        fs::write(outside.path().join("secret"), "x").unwrap();
        std::os::unix::fs::symlink(outside.path(), root.path().join("link")).unwrap();

        let manifest = Manifest::take(root.path()).unwrap();

        assert_eq!(manifest.first_symlink(), Some("link"));
        assert!(!manifest.0.keys().any(|k| k.contains("secret")));
    }
}
