//! The draft PR's title and body.

use crate::events::SentryIssueAlertReceived;

use super::text::{neutralise_mentions, truncate_bytes, truncate_chars};

/// GitHub's title limit.
const MAX_PR_TITLE_CHARS: usize = 200;
/// The body cap, leaving headroom below GitHub's 65536.
const MAX_PR_BODY_BYTES: usize = 60000;

/// Ends every PR body.
const PR_FOOTER: &str = "\n\n---\nOpened by heinzel. Claude Code investigated the issue in a sandbox; review before merging.\n";

/// Names the Sentry issue. Alert text can't @mention anyone.
pub(super) fn pr_title(alert: &SentryIssueAlertReceived) -> String {
    let title = neutralise_mentions(&format!(
        "Fix Sentry issue {}: {}",
        alert.issue_id, alert.title
    ));
    truncate_chars(&title, MAX_PR_TITLE_CHARS).to_owned()
}

/// Links the Sentry issue and carries Claude's summary. Neither the alert's
/// text nor Claude's can @mention anyone. Only the summary is cut to keep
/// the body within [`MAX_PR_BODY_BYTES`], so the footer always survives.
pub(super) fn pr_body(alert: &SentryIssueAlertReceived, summary: &str) -> String {
    let issue = neutralise_mentions(&alert.issue_id);
    let issue = if safe_link(&alert.web_url) {
        format!("[{issue}]({})", alert.web_url)
    } else {
        issue
    };
    let header = format!(
        "Sentry issue {issue}: {}\n\n## Claude's summary\n\n",
        neutralise_mentions(&alert.title)
    );
    let header = truncate_bytes(&header, MAX_PR_BODY_BYTES - PR_FOOTER.len());
    let summary = neutralise_mentions(summary.trim());
    let summary = truncate_summary(
        &summary,
        MAX_PR_BODY_BYTES - header.len() - PR_FOOTER.len(),
    );
    format!("{header}{summary}{PR_FOOTER}")
}

/// Reports whether `url` can be a Markdown link target as it is: an https
/// URL with nothing that could end the link or start markup.
fn safe_link(url: &str) -> bool {
    url.starts_with("https://")
        && !url
            .chars()
            .any(|c| c.is_whitespace() || c.is_control() || "()<>".contains(c))
}

/// Cuts `s` to at most `n` bytes at a char boundary. If the cut leaves a
/// code fence open, it closes it with the fence's own characters, within
/// the `n` bytes.
fn truncate_summary(s: &str, n: usize) -> String {
    if s.len() <= n {
        return s.to_owned();
    }
    let mut cut = truncate_bytes(s, n);
    let mut reserve = 0;
    loop {
        let Some(fence) = open_fence(cut) else {
            return cut.to_owned();
        };
        let closing = format!("\n{fence}");
        if closing.len() <= reserve {
            return format!("{cut}{closing}");
        }
        // Make room for the closing fence and look again: the shorter cut
        // may end in another fence, or in none.
        reserve = closing.len();
        cut = truncate_bytes(s, n.saturating_sub(reserve));
    }
}

/// Returns the opening run of the Markdown code fence (```` ``` ```` or
/// `~~~`, possibly longer) that `s` ends inside, or `None` if `s` ends
/// outside any fence. As in CommonMark, only a run of the same character,
/// at least as long and with nothing after it, closes a fence.
fn open_fence(s: &str) -> Option<&str> {
    let mut open: Option<&str> = None;
    for line in s.split('\n') {
        let line = line.trim_start_matches(' ');
        let Some(run) = fence_run(line) else {
            continue;
        };
        match open {
            None => open = Some(run),
            Some(o) if run.as_bytes()[0] == o.as_bytes()[0]
                && run.len() >= o.len()
                && line[run.len()..].trim().is_empty() =>
            {
                open = None;
            }
            Some(_) => {}
        }
    }
    open
}

/// Returns `line`'s leading run of three or more backticks or tildes.
fn fence_run(line: &str) -> Option<&str> {
    let first = *line.as_bytes().first()?;
    if first != b'`' && first != b'~' {
        return None;
    }
    let n = line.bytes().take_while(|&b| b == first).count();
    (n >= 3).then(|| &line[..n])
}

#[cfg(test)]
mod tests {
    use super::*;

    const ZWSP: &str = "\u{200b}";

    fn alert(issue_id: &str, title: &str, web_url: &str) -> SentryIssueAlertReceived {
        SentryIssueAlertReceived {
            issue_id: issue_id.into(),
            title: title.into(),
            web_url: web_url.into(),
            ..Default::default()
        }
    }

    #[test]
    fn the_title_is_capped_and_mentions_are_neutralised() {
        let alert = alert("42", &format!("Error for @acme/team {}", "x".repeat(300)), "");

        let got = pr_title(&alert);

        assert_eq!(got.chars().count(), MAX_PR_TITLE_CHARS);
        assert!(
            got.starts_with(&format!("Fix Sentry issue 42: Error for @{ZWSP}acme/team ")),
            "{got}"
        );
    }

    #[test]
    fn the_body_neutralises_mentions_and_keeps_the_sentry_link() {
        let alert = alert("42", "ping @oncall", "https://sentry.io/organizations/acme/issues/42/");

        let got = pr_body(&alert, "Fixed it. cc @octocat");

        for want in [
            format!("ping @{ZWSP}oncall"),
            format!("cc @{ZWSP}octocat"),
            "(https://sentry.io/organizations/acme/issues/42/)".to_owned(),
        ] {
            assert!(got.contains(&want), "body lacks {want:?}:\n{got}");
        }
        assert_eq!(got.matches('@').count(), got.matches(&format!("@{ZWSP}")).count());
    }

    #[test]
    fn an_unsafe_sentry_link_is_left_out() {
        for url in [
            "javascript:alert(1)",
            "http://sentry.io/i/42",
            "https://sentry.io/i/42) [click](https://evil.example",
            "https://sentry.io/i/42 x",
            "https://sentry.io/<b>",
            "",
        ] {
            let got = pr_body(&alert("42", "t", url), "s");

            assert!(got.starts_with("Sentry issue 42: t\n\n## Claude's summary\n\ns"), "{url:?}: {got}");
        }
    }

    #[test]
    fn the_body_cuts_only_the_summary_at_a_char_boundary() {
        let alert = alert("42", "t", "https://sentry.io/i/42");

        let got = pr_body(&alert, &"ü".repeat(MAX_PR_BODY_BYTES));

        assert!(got.len() <= MAX_PR_BODY_BYTES && got.len() >= MAX_PR_BODY_BYTES - 4, "{}", got.len());
        assert!(got.starts_with("Sentry issue [42](https://sentry.io/i/42): t\n\n## Claude's summary\n\nüü"));
        assert!(got.ends_with(PR_FOOTER));
    }

    #[test]
    fn the_body_keeps_a_cut_code_fence_closed() {
        let alert = alert("42", "t", "https://sentry.io/i/42");
        let summary = format!("Root cause below.\n\n```go\n{}```\n", "x := 1\n".repeat(MAX_PR_BODY_BYTES / 7));

        let got = pr_body(&alert, &summary);

        assert!(got.len() <= MAX_PR_BODY_BYTES, "{}", got.len());
        assert!(got.ends_with(&format!("\n```{PR_FOOTER}")), "{}", &got[got.len() - 300..]);
        assert_eq!(got.matches("```").count() % 2, 0);
    }

    #[test]
    fn the_body_closes_a_cut_tilde_fence_with_tildes() {
        let alert = alert("42", "t", "https://sentry.io/i/42");
        let summary = format!("Root cause below.\n\n~~~go\n```\n{}~~~\n", "x := 1\n".repeat(MAX_PR_BODY_BYTES / 7));

        let got = pr_body(&alert, &summary);

        assert!(got.len() <= MAX_PR_BODY_BYTES, "{}", got.len());
        assert!(got.ends_with(&format!("\n~~~{PR_FOOTER}")), "{}", &got[got.len() - 300..]);
    }

    #[test]
    fn the_body_leaves_a_short_summary_whole() {
        let alert = alert("42", "t", "https://sentry.io/i/42");

        let got = pr_body(&alert, "  ```\ncode\n```\n");

        assert_eq!(
            got,
            format!("Sentry issue [42](https://sentry.io/i/42): t\n\n## Claude's summary\n\n```\ncode\n```{PR_FOOTER}")
        );
    }

    #[test]
    fn a_huge_header_is_cut_so_the_footer_survives() {
        let alert = alert("42", &"t".repeat(MAX_PR_BODY_BYTES), "u");

        let got = pr_body(&alert, "summary");

        assert_eq!(got.len(), MAX_PR_BODY_BYTES);
        assert!(got.ends_with(PR_FOOTER));
    }

    #[test]
    fn open_fence_follows_commonmark() {
        let cases = [
            ("text", None),
            ("```\ncode", Some("```")),
            ("```\ncode\n```", None),
            ("~~~\ncode", Some("~~~")),
            ("~~~\ncode\n~~~", None),
            ("~~~\n```\ncode", Some("~~~")), // ``` doesn't close a tilde fence
            ("```\n~~~\ncode", Some("```")),
            ("  ~~~~\ncode\n~~~", Some("~~~~")), // a shorter run doesn't close it
            ("~~~~\ncode\n~~~~~\nafter", None),
            ("```\ncode\n``` x", Some("```")), // text after the run doesn't close
            ("``\ncode", None),
        ];
        for (s, want) in cases {
            assert_eq!(open_fence(s), want, "{s:?}");
        }
    }
}
