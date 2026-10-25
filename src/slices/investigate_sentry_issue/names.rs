//! How one Sentry issue's investigation stream and branch are named.

use sha2::{Digest, Sha256};

/// The issue id as used in stream and branch names: as-is when it is
/// `[A-Za-z0-9_-]+`, otherwise its hex sha256.
fn issue_key(issue_id: &str) -> String {
    let plain = !issue_id.is_empty()
        && issue_id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-');
    if plain {
        issue_id.to_owned()
    } else {
        hex::encode(Sha256::digest(issue_id.as_bytes()))
    }
}

/// Names the investigation stream of one Sentry issue.
pub(super) fn stream_id(issue_id: &str) -> String {
    format!("sentry-issue-investigation-{}", issue_key(issue_id))
}

/// Names the branch Claude works on for one Sentry issue.
pub(super) fn branch_name(issue_id: &str) -> String {
    format!("heinzel/sentry-{}", issue_key(issue_id))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stream_and_branch_names() {
        let cases = [
            (
                "1117540176",
                "sentry-issue-investigation-1117540176",
                "heinzel/sentry-1117540176",
            ),
            (
                "PROJ-1_a",
                "sentry-issue-investigation-PROJ-1_a",
                "heinzel/sentry-PROJ-1_a",
            ),
            // sha256("a/b")
            (
                "a/b",
                "sentry-issue-investigation-c14cddc033f64b9dea80ea675cf280a015e672516090a5626781153dc68fea11",
                "heinzel/sentry-c14cddc033f64b9dea80ea675cf280a015e672516090a5626781153dc68fea11",
            ),
            // sha256("")
            (
                "",
                "sentry-issue-investigation-e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
                "heinzel/sentry-e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
            ),
        ];
        for (issue_id, stream, branch) in cases {
            assert_eq!(stream_id(issue_id), stream, "{issue_id:?}");
            assert_eq!(branch_name(issue_id), branch, "{issue_id:?}");
        }
    }
}
