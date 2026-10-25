//! The work dir that holds the run dirs, and the check that keeps other
//! users out of it.

use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::Path;

use super::super::paths::clean;
use super::Error;

/// A work dir of this user's own in the shared OS temp dir.
pub(crate) fn default_dir() -> String {
    let temp = std::env::temp_dir();
    clean(&format!(
        "{}/heinzel-investigations-{}",
        temp.to_string_lossy(),
        nix::unistd::getuid()
    ))
}

/// Fails unless `dir` is a real directory (not a symlink), owned by this
/// user, that no group or other user can use. In a shared temp dir, another
/// user could have created it first to read or plant run dirs.
pub(crate) fn check_dir(dir: &str) -> Result<(), Error> {
    let info = Path::new(dir)
        .symlink_metadata()
        .map_err(|err| Error(format!("check work dir: {err}")))?;
    if info.file_type().is_symlink() {
        return Err(Error(format!(
            "work dir {dir} is a symlink; use a real directory"
        )));
    }
    if !info.is_dir() {
        return Err(Error(format!("work dir {dir} is not a directory")));
    }
    let uid = nix::unistd::getuid().as_raw();
    if info.uid() != uid {
        return Err(Error(format!(
            "work dir {dir} is not owned by this user (uid {uid})"
        )));
    }
    let perm = info.permissions().mode() & 0o777;
    if perm & 0o077 != 0 {
        return Err(Error(format!(
            "work dir {dir} has mode 0{perm:o}; it must give the group and others no access (e.g. 0700)"
        )));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::os::unix::fs::DirBuilderExt;

    use super::*;

    fn private_dir(root: &Path) -> String {
        let dir = root.join("work");
        fs::DirBuilder::new().mode(0o700).create(&dir).unwrap();
        dir.to_str().unwrap().to_owned()
    }

    #[test]
    fn the_default_work_dir_is_per_user() {
        let want = clean(&format!(
            "{}/heinzel-investigations-{}",
            std::env::temp_dir().display(),
            nix::unistd::getuid()
        ));

        assert_eq!(default_dir(), want);
    }

    #[test]
    fn a_private_dir_of_our_own_passes() {
        let root = tempfile::tempdir().unwrap();

        check_dir(&private_dir(root.path())).unwrap();
    }

    #[test]
    fn a_symlink_is_rejected() {
        let root = tempfile::tempdir().unwrap();
        let target = private_dir(root.path());
        let link = root.path().join("link");
        std::os::unix::fs::symlink(&target, &link).unwrap();
        let link = link.to_str().unwrap();

        let err = check_dir(link).unwrap_err().to_string();

        assert_eq!(err, format!("work dir {link} is a symlink; use a real directory"));
    }

    #[test]
    fn a_dir_others_can_use_is_rejected() {
        let root = tempfile::tempdir().unwrap();
        let dir = private_dir(root.path());
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o755)).unwrap();

        let err = check_dir(&dir).unwrap_err().to_string();

        assert_eq!(
            err,
            format!("work dir {dir} has mode 0755; it must give the group and others no access (e.g. 0700)")
        );
    }

    #[test]
    fn a_file_is_rejected() {
        let root = tempfile::tempdir().unwrap();
        let file = root.path().join("work");
        fs::write(&file, "").unwrap();
        let file = file.to_str().unwrap();

        let err = check_dir(file).unwrap_err().to_string();

        assert_eq!(err, format!("work dir {file} is not a directory"));
    }
}
