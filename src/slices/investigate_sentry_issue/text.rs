//! Cutting and defusing text that came from an alert or from Claude.

/// Returns `s` cut to at most `n` bytes, at a char boundary.
pub(super) fn truncate_bytes(s: &str, n: usize) -> &str {
    if s.len() <= n {
        return s;
    }
    let mut end = n;
    while !s.is_char_boundary(end) {
        end -= 1;
    }
    &s[..end]
}

/// Returns `s` cut to at most `n` chars.
pub(super) fn truncate_chars(s: &str, n: usize) -> &str {
    s.char_indices().nth(n).map_or(s, |(i, _)| &s[..i])
}

/// Puts a zero-width space after every `@`, and inside every HTML entity
/// for `@` (`&#64;`, `&#x40;`, `&commat;`, in any case), so text that came
/// from an alert or from Claude can't @mention anyone on GitHub.
pub(super) fn neutralise_mentions(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for (i, c) in s.char_indices() {
        out.push(c);
        if c == '@' || (c == '&' && at_entity(&s[i + 1..])) {
            out.push('\u{200b}');
        }
    }
    out
}

/// Reports whether `rest`, the text after an `&`, starts an entity for `@`:
/// `commat`, or `#64` or `#x40` with any number of leading zeros.
fn at_entity(rest: &str) -> bool {
    if rest.get(..6).is_some_and(|name| name.eq_ignore_ascii_case("commat")) {
        return true;
    }
    let Some(number) = rest.strip_prefix('#') else {
        return false;
    };
    let (digits, want) = match number.strip_prefix(['x', 'X']) {
        Some(hex) => (hex, "40"),
        None => (number, "64"),
    };
    let digits = digits.trim_start_matches('0');
    digits.starts_with(want)
        && !digits[want.len()..].starts_with(|c: char| c.is_ascii_hexdigit() && (want == "40" || c.is_ascii_digit()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn truncate_bytes_cuts_at_a_char_boundary() {
        assert_eq!(truncate_bytes("abc", 3), "abc");
        assert_eq!(truncate_bytes("abc", 2), "ab");
        assert_eq!(truncate_bytes("aü", 2), "a"); // ü is two bytes
        assert_eq!(truncate_bytes("aü", 3), "aü");
        assert_eq!(truncate_bytes("ü", 0), "");
    }

    #[test]
    fn truncate_chars_counts_chars() {
        assert_eq!(truncate_chars("éé", 1), "é");
        assert_eq!(truncate_chars("éé", 2), "éé");
        assert_eq!(truncate_chars("éé", 5), "éé");
        assert_eq!(truncate_chars("éé", 0), "");
    }

    #[test]
    fn entities_for_at_are_broken_up() {
        let cases = [
            ("&#64;team", "&\u{200b}#64;team"),
            ("&#064;team", "&\u{200b}#064;team"),
            ("&#x40;team", "&\u{200b}#x40;team"),
            ("&#X40;team", "&\u{200b}#X40;team"),
            ("&commat;team", "&\u{200b}commat;team"),
            ("&COMMAT;team", "&\u{200b}COMMAT;team"),
            ("&amp; &#65; &#640; &#x400; &#x40a;", "&amp; &#65; &#640; &#x400; &#x40a;"),
            ("&#64a", "&\u{200b}#64a"),
            (
                &format!("&#{}64;team", "0".repeat(40)),
                &format!("&\u{200b}#{}64;team", "0".repeat(40)),
            ),
            (
                &format!("&#x{}40;team", "0".repeat(40)),
                &format!("&\u{200b}#x{}40;team", "0".repeat(40)),
            ),
            ("a & b &", "a & b &"),
        ];
        for (s, want) in cases {
            assert_eq!(neutralise_mentions(s), want, "{s:?}");
        }
    }

    #[test]
    fn mentions_get_a_zero_width_space() {
        assert_eq!(neutralise_mentions("a @b @@c"), "a @\u{200b}b @\u{200b}@\u{200b}c");
    }
}
