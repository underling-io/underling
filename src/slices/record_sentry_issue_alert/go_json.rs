//! Decoding rules of Go's `encoding/json` that the webhook's contract relies
//! on: a missing or `null` value is empty, a struct decodes only from an
//! object, and a value of the wrong type is an error.

use std::fmt;
use std::marker::PhantomData;

use serde::de::value::MapAccessDeserializer;
use serde::de::{Error, MapAccess, Visitor};
use serde::{Deserialize, Deserializer};

/// Deserializes a `null` value as its default. Use it with
/// `#[serde(default)]`, so a missing value is its default too.
pub(super) fn null_as_default<'de, D, T>(deserializer: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Option::<T>::deserialize(deserializer).map(Option::unwrap_or_default)
}

/// A struct decoded as Go decodes one: from an object, or empty from `null`.
/// Anything else is an error, where serde's derive would also accept an
/// array of the fields. It decodes straight from the input, with no
/// intermediate `serde_json::Value`, so fields it ignores are only skipped.
#[derive(Debug, Default)]
pub(super) struct Object<T>(pub(super) T);

impl<'de, T> Deserialize<'de> for Object<T>
where
    T: Deserialize<'de> + Default,
{
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        deserializer.deserialize_any(ObjectVisitor(PhantomData))
    }
}

struct ObjectVisitor<T>(PhantomData<T>);

impl<'de, T> Visitor<'de> for ObjectVisitor<T>
where
    T: Deserialize<'de> + Default,
{
    type Value = Object<T>;

    fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("an object or null")
    }

    fn visit_unit<E: Error>(self) -> Result<Self::Value, E> {
        Ok(Object(T::default()))
    }

    fn visit_none<E: Error>(self) -> Result<Self::Value, E> {
        Ok(Object(T::default()))
    }

    fn visit_map<A: MapAccess<'de>>(self, map: A) -> Result<Self::Value, A::Error> {
        T::deserialize(MapAccessDeserializer::new(map)).map(Object)
    }
}

/// Returns `body` with every unpaired UTF-16 surrogate escape (`\ud800` to
/// `\udfff`) inside a JSON string replaced by `\ufffd`, as Go's decoder
/// does. serde_json rejects such escapes, but Sentry sends them: a message
/// cut mid-emoji keeps half a surrogate pair. Valid pairs, escaped
/// backslashes and malformed input are left alone.
pub(super) fn replace_lone_surrogates(body: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(body.len());
    let mut in_string = false;
    let mut i = 0;
    while i < body.len() {
        let byte = body[i];
        if !in_string {
            in_string = byte == b'"';
            out.push(byte);
            i += 1;
            continue;
        }
        match byte {
            b'"' => {
                in_string = false;
                out.push(byte);
                i += 1;
            }
            b'\\' => match surrogate_at(body, i) {
                // A high surrogate followed by a low one: a valid pair.
                Some(0xD800..=0xDBFF)
                    if surrogate_at(body, i + 6).is_some_and(|low| low >= 0xDC00) =>
                {
                    out.extend_from_slice(&body[i..i + 12]);
                    i += 12;
                }
                Some(_) => {
                    out.extend_from_slice(br"\ufffd");
                    i += 6;
                }
                // Any other escape, copied whole so an escaped backslash or
                // quote isn't read as the start of another.
                None => {
                    let end = (i + 2).min(body.len());
                    out.extend_from_slice(&body[i..end]);
                    i = end;
                }
            },
            _ => {
                out.push(byte);
                i += 1;
            }
        }
    }
    out
}

/// The code unit of the `\uXXXX` escape at `i`, if it is a surrogate.
fn surrogate_at(body: &[u8], i: usize) -> Option<u16> {
    let hex = body.get(i..i + 6)?.strip_prefix(br"\u")?;
    if !hex.iter().all(u8::is_ascii_hexdigit) {
        return None;
    }
    let unit = u16::from_str_radix(std::str::from_utf8(hex).ok()?, 16).ok()?;
    (0xD800..=0xDFFF).contains(&unit).then_some(unit)
}

#[cfg(test)]
mod tests {
    use super::replace_lone_surrogates;

    fn replaced(json: &str) -> String {
        String::from_utf8(replace_lone_surrogates(json.as_bytes())).unwrap()
    }

    #[test]
    fn a_lone_surrogate_escape_becomes_the_replacement_character() {
        assert_eq!(replaced(r#"{"t":"A\udc00"}"#), r#"{"t":"A\ufffd"}"#);
        assert_eq!(replaced(r#"{"t":"\uD83D!"}"#), r#"{"t":"\ufffd!"}"#);
        assert_eq!(replaced(r#"{"\udc00":1}"#), r#"{"\ufffd":1}"#);
        assert_eq!(replaced(r#"["\ude00\ude00"]"#), r#"["\ufffd\ufffd"]"#);
    }

    #[test]
    fn a_high_surrogate_not_followed_by_a_low_one_is_replaced_alone() {
        assert_eq!(
            replaced(r#"["\ud83d\ud83d\ude00", "\ud83d\u0041", "\ud83d\n"]"#),
            r#"["\ufffd\ud83d\ude00", "\ufffd\u0041", "\ufffd\n"]"#
        );
    }

    #[test]
    fn valid_pairs_and_other_escapes_are_kept() {
        let json = r#"{"t":"\ud83d\ude00 \u00fc \" \\ \/ \n","n":[1,true,null]}"#;

        assert_eq!(replaced(json), json);
    }

    #[test]
    fn an_escaped_backslash_before_u_is_no_escape() {
        let json = r#"{"t":"\\udc00","u":"\\\udc00"}"#;

        assert_eq!(replaced(json), r#"{"t":"\\udc00","u":"\\\ufffd"}"#);
    }

    #[test]
    fn malformed_escapes_are_left_for_the_decoder() {
        for json in [r#"{"t":"\ud"#, r#"{"t":"\udczz"}"#, r#"{"t":"\"#] {
            assert_eq!(replaced(json), json);
        }
    }

    #[test]
    fn text_outside_strings_is_left_alone() {
        let json = r#" {"a" : [ "\\" , 1 ] } \udc00"#;

        assert_eq!(replaced(json), json);
    }
}
