//! Parses durations as Go's `time.ParseDuration` does, so the config takes
//! exactly what the Go version took: `30m`, `1h30m`, `1.5h`, `-2m3.4s`.

/// A signed number of nanoseconds, like Go's `time.Duration`.
pub(super) type Nanos = i64;

/// Parses `s`: an optional sign, then one or more decimal numbers, each
/// with a unit (`ns`, `us`, `µs`, `μs`, `ms`, `s`, `m`, `h`). `0` alone
/// needs no unit. Errors read like Go's.
pub(super) fn parse_go_duration(s: &str) -> Result<Nanos, String> {
    const MAX: u64 = 1 << 63;
    let invalid = || format!("time: invalid duration {s:?}");
    let (negative, mut rest) = match s.as_bytes().first() {
        Some(b'-') => (true, &s[1..]),
        Some(b'+') => (false, &s[1..]),
        _ => (false, s),
    };
    if rest == "0" {
        return Ok(0);
    }
    if rest.is_empty() {
        return Err(invalid());
    }
    let mut total: u64 = 0;
    while !rest.is_empty() {
        // The number: [0-9]*(\.[0-9]*)?, with digits on at least one side.
        let (int, after) = leading_int(rest).ok_or_else(invalid)?;
        let has_int = after.len() != rest.len();
        let (frac, scale, after, has_frac) = match after.strip_prefix('.') {
            Some(digits) => {
                let (frac, scale, after) = leading_fraction(digits);
                (frac, scale, after, after.len() != digits.len())
            }
            None => (0, 1.0, after, false),
        };
        if !has_int && !has_frac {
            return Err(invalid());
        }
        let unit_len = after
            .char_indices()
            .find(|(_, c)| *c == '.' || c.is_ascii_digit())
            .map_or(after.len(), |(i, _)| i);
        let (unit, after) = after.split_at(unit_len);
        if unit.is_empty() {
            return Err(format!("time: missing unit in duration {s:?}"));
        }
        let scale_ns: u64 = match unit {
            "ns" => 1,
            "us" | "µs" | "μs" => 1_000,
            "ms" => 1_000_000,
            "s" => 1_000_000_000,
            "m" => 60 * 1_000_000_000,
            "h" => 3600 * 1_000_000_000,
            _ => return Err(format!("time: unknown unit {unit:?} in duration {s:?}")),
        };
        if int > MAX / scale_ns {
            return Err(invalid());
        }
        let mut value = int * scale_ns;
        if frac > 0 {
            // As Go does it, in floating point.
            value += (frac as f64 * (scale_ns as f64 / scale)) as u64;
            if value > MAX {
                return Err(invalid());
            }
        }
        total = total.checked_add(value).filter(|t| *t <= MAX).ok_or_else(invalid)?;
        rest = after;
    }
    if negative {
        // -2^63 fits; anything up to it negates cleanly.
        return Ok((total as i128).wrapping_neg() as Nanos);
    }
    Nanos::try_from(total).map_err(|_| invalid())
}

/// Consumes leading digits, failing past 2^63, as Go's `leadingInt` does.
fn leading_int(s: &str) -> Option<(u64, &str)> {
    let digits = s.bytes().take_while(u8::is_ascii_digit).count();
    let mut x: u64 = 0;
    for c in s[..digits].bytes() {
        if x > (1 << 63) / 10 {
            return None;
        }
        x = x * 10 + u64::from(c - b'0');
        if x > 1 << 63 {
            return None;
        }
    }
    Some((x, &s[digits..]))
}

/// Consumes leading fraction digits, as Go's `leadingFraction` does: it
/// stops accumulating, without failing, once more digits would overflow,
/// and returns the digits' value and their scale (10^n).
fn leading_fraction(s: &str) -> (u64, f64, &str) {
    let digits = s.bytes().take_while(u8::is_ascii_digit).count();
    let (mut x, mut scale) = (0u64, 1.0f64);
    for c in s[..digits].bytes() {
        if x > ((1u64 << 63) - 1) / 10 {
            break;
        }
        let y = x * 10 + u64::from(c - b'0');
        if y > 1 << 63 {
            break;
        }
        x = y;
        scale *= 10.0;
    }
    (x, scale, &s[digits..])
}

#[cfg(test)]
mod tests {
    use super::*;

    const S: i64 = 1_000_000_000;

    /// The cases of Go's `TestParseDuration`.
    #[test]
    fn parses_what_go_parses() {
        let cases: &[(&str, i64)] = &[
            ("0", 0),
            ("5s", 5 * S),
            ("30s", 30 * S),
            ("1478s", 1478 * S),
            ("-5s", -5 * S),
            ("+5s", 5 * S),
            ("-0", 0),
            ("+0", 0),
            ("5.0s", 5 * S),
            ("5.6s", 5 * S + 600_000_000),
            ("5.s", 5 * S),
            (".5s", 500_000_000),
            ("1.0s", S),
            ("1.00s", S),
            ("1.004s", S + 4_000_000),
            ("1.0040s", S + 4_000_000),
            ("100.00100s", 100 * S + 1_000_000),
            ("10ns", 10),
            ("11us", 11_000),
            ("12µs", 12_000),
            ("12μs", 12_000),
            ("13ms", 13_000_000),
            ("14s", 14 * S),
            ("15m", 15 * 60 * S),
            ("16h", 16 * 3600 * S),
            ("3h30m", (3 * 3600 + 30 * 60) * S),
            ("10.5s4m", 4 * 60 * S + 10 * S + 500_000_000),
            ("-2m3.4s", -(2 * 60 * S + 3 * S + 400_000_000)),
            ("1h2m3s4ms5us6ns", 3600 * S + 2 * 60 * S + 3 * S + 4_000_000 + 5_000 + 6),
            ("39h9m14.425s", (39 * 3600 + 9 * 60 + 14) * S + 425_000_000),
            ("52763797000ns", 52_763_797_000),
            ("0.3333333333333333333h", 20 * 60 * S),
            ("9007199254740993ns", 9_007_199_254_740_993),
            ("9223372036854775807ns", i64::MAX),
            ("9223372036854775.807us", i64::MAX),
            ("9223372036s854ms775us807ns", i64::MAX),
            ("-9223372036s854ms775us808ns", i64::MIN),
            ("-9223372036854775808ns", i64::MIN),
            ("-9223372036854775.808us", i64::MIN),
            ("-2562047h47m16.854775808s", i64::MIN),
            ("0.100000000000000000000h", 6 * 60 * S),
            ("0.830103483285477580700h", 49 * 60 * S + 48_372_539_827),
        ];
        for (input, want) in cases {
            assert_eq!(parse_go_duration(input), Ok(*want), "{input:?}");
        }
    }

    #[test]
    fn rejects_what_go_rejects() {
        let cases: &[(&str, &str)] = &[
            ("", r#"time: invalid duration """#),
            ("3", r#"time: missing unit in duration "3""#),
            ("-", r#"time: invalid duration "-""#),
            ("s", r#"time: invalid duration "s""#),
            (".", r#"time: invalid duration ".""#),
            ("-.", r#"time: invalid duration "-.""#),
            (".s", r#"time: invalid duration ".s""#),
            ("+.s", r#"time: invalid duration "+.s""#),
            ("1d", r#"time: unknown unit "d" in duration "1d""#),
            ("1 s", r#"time: unknown unit " s" in duration "1 s""#),
            ("30 m", r#"time: unknown unit " m" in duration "30 m""#),
            ("9223372036854775810ns", r#"time: invalid duration "9223372036854775810ns""#),
            ("9223372036854775808ns", r#"time: invalid duration "9223372036854775808ns""#),
            ("-9223372036854775809ns", r#"time: invalid duration "-9223372036854775809ns""#),
            ("3000000h", r#"time: invalid duration "3000000h""#),
            ("9223372036854775.808us", r#"time: invalid duration "9223372036854775.808us""#),
            ("9223372036s854ms775us808ns", r#"time: invalid duration "9223372036s854ms775us808ns""#),
            ("9223372036854s", r#"time: invalid duration "9223372036854s""#),
        ];
        for (input, want) in cases {
            assert_eq!(parse_go_duration(input), Err((*want).to_owned()), "{input:?}");
        }
    }
}
