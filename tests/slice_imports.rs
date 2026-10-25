//! Slices may use only std, external crates, the shared event contracts and
//! the generic infrastructure (`crate::events`, `crate::eventstore`,
//! `crate::eventhandler`, `crate::app`), and their own sub-modules. So slices
//! stay coupled only through events.

#[path = "../build/slice_dirs.rs"]
mod slice_dirs;

use std::path::{Component, Path};

use proc_macro2::{Delimiter, TokenStream, TokenTree};
use syn::visit::{self, Visit};

/// The crate modules every slice may use.
const ALLOWED: [&str; 4] = ["events", "eventstore", "eventhandler", "app"];

/// Checks one source file of `slice` whose module sits `depth` levels below
/// the slice's root module, and returns its violations.
fn violations(source: &str, slice: &str, depth: usize) -> Vec<String> {
    let file = syn::parse_file(source).unwrap_or_else(|err| panic!("parse: {err}"));
    let mut checker = Checker {
        slice,
        depth,
        violations: Vec::new(),
    };
    checker.visit_file(&file);
    checker.violations
}

struct Checker<'a> {
    slice: &'a str,
    depth: usize,
    violations: Vec<String>,
}

impl Checker<'_> {
    fn check(&mut self, segments: &[String]) {
        // `self::super::x` is `super::x`.
        let unprefixed = {
            let selfs = segments.iter().take_while(|s| *s == "self").count();
            &segments[selfs..]
        };
        let allowed = match unprefixed {
            [first, second, rest @ ..] if first == "crate" => {
                ALLOWED.contains(&second.as_str())
                    || (second == "slices" && rest.first().is_some_and(|s| s == self.slice))
            }
            // The bare crate root, as in `use crate as root;`, reaches
            // everything.
            [first] if first == "crate" => false,
            [first, ..] if first == "super" => {
                unprefixed.iter().take_while(|s| *s == "super").count() <= self.depth
            }
            _ => true,
        };
        if !allowed {
            self.violations.push(segments.join("::"));
        }
    }
}

impl<'ast> Visit<'ast> for Checker<'_> {
    fn visit_path(&mut self, path: &'ast syn::Path) {
        let segments: Vec<String> = path.segments.iter().map(|s| s.ident.to_string()).collect();
        self.check(&segments);
        visit::visit_path(self, path);
    }

    fn visit_use_tree(&mut self, tree: &'ast syn::UseTree) {
        for segments in use_paths(tree) {
            // An alias like `use std::include as inc;` would dodge the
            // include checks on macro names.
            if segments
                .last()
                .is_some_and(|s| matches!(s.as_str(), "include" | "include_str" | "include_bytes"))
            {
                self.violations.push(format!("use {}", segments.join("::")));
            }
            self.check(&segments);
        }
    }

    // `pub(crate)` and the like restrict visibility; they reach nothing.
    fn visit_visibility(&mut self, _: &'ast syn::Visibility) {}

    // `extern crate self as root;` names the crate root, so it reaches
    // everything.
    fn visit_item_extern_crate(&mut self, item: &'ast syn::ItemExternCrate) {
        if item.ident == "self" {
            self.violations.push("extern crate self".to_owned());
        }
        visit::visit_item_extern_crate(self, item);
    }

    fn visit_item_mod(&mut self, item: &'ast syn::ItemMod) {
        self.depth += 1;
        visit::visit_item_mod(self, item);
        self.depth -= 1;
    }

    // `#[path]` points a module at any file, even another slice's.
    fn visit_attribute(&mut self, attr: &'ast syn::Attribute) {
        if attr.path().is_ident("path") {
            self.violations.push("#[path]".to_owned());
        }
        // `#[cfg_attr(all(), path = "…")]` is a `#[path]` too.
        if attr.path().is_ident("cfg_attr")
            && let syn::Meta::List(list) = &attr.meta
        {
            let mut flat = Vec::new();
            flatten(list.tokens.clone(), &mut flat);
            let sets_path = flat.windows(2).any(|pair| {
                matches!(&pair[0], TokenTree::Ident(id) if id == "path")
                    && matches!(&pair[1], TokenTree::Punct(p) if p.as_char() == '=')
            });
            if sets_path {
                self.violations.push("#[cfg_attr(…, path = …)]".to_owned());
            }
        }
        visit::visit_attribute(self, attr);
    }

    fn visit_macro(&mut self, mac: &'ast syn::Macro) {
        let mut flat = Vec::new();
        flatten(mac.tokens.clone(), &mut flat);
        if let Some(name) = mac.path.segments.last() {
            let arg = mac.tokens.clone().into_iter().next();
            self.violations
                .extend(include_violation(&name.ident.to_string(), arg.as_ref()));
        }
        for (i, token) in flat.iter().enumerate() {
            if let TokenTree::Ident(name) = token
                && matches!(flat.get(i + 1), Some(TokenTree::Punct(p)) if p.as_char() == '!')
            {
                self.violations
                    .extend(include_violation(&name.to_string(), flat.get(i + 2)));
            }
        }
        for segments in token_paths(&flat) {
            self.check(&segments);
        }
        visit::visit_macro(self, mac);
    }
}

/// Checks a call of macro `name` whose first argument token is `arg`.
/// `include!` pulls in code from anywhere, so it's never allowed.
/// `include_str!` and `include_bytes!` only embed data, so they may read a
/// relative path that doesn't climb up with `..`.
fn include_violation(name: &str, arg: Option<&TokenTree>) -> Option<String> {
    match name {
        "include" => Some("include!".to_owned()),
        "include_str" | "include_bytes" => {
            let path = match arg {
                Some(TokenTree::Literal(lit)) => syn::parse_str::<syn::LitStr>(&lit.to_string())
                    .ok()
                    .map(|lit| lit.value()),
                _ => None,
            };
            match path {
                Some(path)
                    if Path::new(&path)
                        .components()
                        .all(|c| matches!(c, Component::Normal(_) | Component::CurDir)) =>
                {
                    None
                }
                Some(path) => Some(format!("{name}!({path:?})")),
                None => Some(format!("{name}!(…)")),
            }
        }
        _ => None,
    }
}

/// Returns every path a use tree imports, as segments.
fn use_paths(tree: &syn::UseTree) -> Vec<Vec<String>> {
    match tree {
        syn::UseTree::Path(path) => use_paths(&path.tree)
            .into_iter()
            .map(|rest| {
                std::iter::once(path.ident.to_string())
                    .chain(rest)
                    .collect()
            })
            .collect(),
        syn::UseTree::Name(name) => vec![vec![name.ident.to_string()]],
        syn::UseTree::Rename(rename) => vec![vec![rename.ident.to_string()]],
        syn::UseTree::Glob(_) => vec![vec![]],
        syn::UseTree::Group(group) => group.items.iter().flat_map(use_paths).collect(),
    }
}

/// Returns the `crate::…`, `super::…` and `self::…` paths in a macro's
/// tokens, which syn doesn't parse.
fn token_paths(flat: &[TokenTree]) -> Vec<Vec<String>> {
    let mut paths = Vec::new();
    let mut i = 0;
    while i < flat.len() {
        let starts_path = matches!(&flat[i], TokenTree::Ident(id) if id == "crate" || id == "super" || id == "self");
        let after_colons = i >= 2 && is_colon(&flat[i - 1]) && is_colon(&flat[i - 2]);
        if !starts_path || after_colons {
            i += 1;
            continue;
        }
        let mut segments = vec![flat[i].to_string()];
        i += 1;
        while i + 1 < flat.len() && is_colon(&flat[i]) && is_colon(&flat[i + 1]) {
            match flat.get(i + 2) {
                Some(TokenTree::Ident(id)) => segments.push(id.to_string()),
                _ => break,
            }
            i += 3;
        }
        paths.push(segments);
    }
    paths
}

/// Flattens `tokens` into `out`, dropping visibility restrictions such as
/// `pub(crate)`, which reach nothing.
fn flatten(tokens: TokenStream, out: &mut Vec<TokenTree>) {
    for token in tokens {
        match token {
            TokenTree::Group(group)
                if group.delimiter() == Delimiter::Parenthesis
                    && matches!(out.last(), Some(TokenTree::Ident(id)) if id == "pub")
                    && is_restriction(group.stream()) => {}
            TokenTree::Group(group) => flatten(group.stream(), out),
            other => out.push(other),
        }
    }
}

/// Whether `tokens`, found in parentheses after `pub`, are a visibility
/// restriction (`crate`, `self`, `super` or `in path`) rather than, say, a
/// tuple type in a tuple struct field.
fn is_restriction(tokens: TokenStream) -> bool {
    let tokens: Vec<TokenTree> = tokens.into_iter().collect();
    match tokens.as_slice() {
        [TokenTree::Ident(id)] => id == "crate" || id == "self" || id == "super",
        [TokenTree::Ident(id), ..] => id == "in",
        _ => false,
    }
}

fn is_colon(token: &TokenTree) -> bool {
    matches!(token, TokenTree::Punct(p) if p.as_char() == ':')
}

/// Checks every `.rs` file of the slice in `dir`, keyed by file.
fn check_slice(dir: &Path, slice: &str) -> Vec<String> {
    let mut found = Vec::new();
    let mut pending = vec![dir.to_owned()];
    while let Some(next) = pending.pop() {
        for entry in std::fs::read_dir(&next).unwrap() {
            let path = entry.unwrap().path();
            if path.is_dir() {
                pending.push(path);
                continue;
            }
            if path.extension().is_none_or(|ext| ext != "rs") {
                continue;
            }
            let relative = path.strip_prefix(dir).unwrap();
            let dirs = relative.components().count() - 1;
            let depth = if relative.ends_with("mod.rs") {
                dirs
            } else {
                dirs + 1
            };
            let source = std::fs::read_to_string(&path).unwrap();
            for violation in violations(&source, slice, depth) {
                found.push(format!("{} uses {violation}", path.display()));
            }
        }
    }
    found
}

#[test]
fn slices_use_only_shared_contracts_infrastructure_and_their_own_modules() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("src/slices");
    for slice in slice_dirs::slice_dirs(&root).unwrap() {
        let found = check_slice(&root.join(&slice), &slice);
        assert!(
            found.is_empty(),
            "slices may only use std, external crates, their own modules and crate::{{{}}}:\n{}",
            ALLOWED.join(", "),
            found.join("\n"),
        );
    }
}

#[test]
fn the_checker_allows_shared_modules_external_crates_and_own_modules() {
    let source = r#"
        use std::sync::Arc;
        use serde::Deserialize;
        use crate::eventstore::Store;
        use crate::{app, eventhandler::Handler};
        use crate::events::*;
        use crate::slices::me::helper;
        use self::helper::x;
        pub(crate) fn f() { crate::events::X; println!("{}", crate::app::Y); }
        mod inner { use super::helper; pub(super) fn g() { self::super::helper::h(); } }
        pub(in crate::slices::me) struct S;
        extern crate serde as serde_crate;
        macro_rules! m { () => { pub(crate) fn h() { self.x(); } pub(in crate::slices::me) struct T; }; }
        const PROMPT: &str = include_str!("prompt.txt");
        const DATA: &[u8] = include_bytes!("./data/x.bin");
        fn p() { println!("{}", include_str!("prompt.txt")); }
    "#;

    assert_eq!(violations(source, "me", 0), Vec::<String>::new());
    assert_eq!(
        violations("use super::helper;", "me", 1),
        Vec::<String>::new()
    );
}

#[test]
fn the_checker_catches_violations() {
    let cases = [
        ("use crate::logging;", 0, "crate::logging"),
        (
            "use crate::{app, testlog::Logs};",
            0,
            "crate::testlog::Logs",
        ),
        ("use crate::slices::other::X;", 0, "crate::slices::other::X"),
        (
            "fn f() { crate::logging::json(); }",
            0,
            "crate::logging::json",
        ),
        (
            "fn f() -> crate::logging::X { todo!() }",
            0,
            "crate::logging::X",
        ),
        (
            "fn f() { println!(\"{}\", crate::logging::X); }",
            0,
            "crate::logging::X",
        ),
        ("use super::other;", 0, "super::other"),
        ("use super::super::x;", 1, "super::super::x"),
        ("mod inner { use super::super::x; }", 0, "super::super::x"),
        ("use crate as root;", 0, "crate"),
        ("use crate::{self as root};", 0, "crate::self"),
        ("extern crate self as root;", 0, "extern crate self"),
        ("use self::super::x;", 0, "self::super::x"),
        ("use self::super::super::x;", 1, "self::super::super::x"),
        ("fn f() { self::super::x(); }", 0, "self::super::x"),
        (
            "fn f() { println!(\"{}\", self::super::X); }",
            0,
            "self::super::X",
        ),
        (
            "m! { struct S(pub (crate::logging::T, u8)); }",
            0,
            "crate::logging::T",
        ),
        ("#[path = \"../other/mod.rs\"] mod other;", 0, "#[path]"),
        ("include!(\"helper.rs\");", 0, "include!"),
        (
            "fn f() { let _ = vec![include!(\"x.rs\")]; }",
            0,
            "include!",
        ),
        (
            "const S: &str = include_str!(\"../other/x.txt\");",
            0,
            "include_str!(\"../other/x.txt\")",
        ),
        (
            "fn f() { println!(\"{}\", include_bytes!(\"/etc/passwd\").len()); }",
            0,
            "include_bytes!(\"/etc/passwd\")",
        ),
        (
            "const S: &str = include_str!(concat!(\"..\", \"/x\"));",
            0,
            "include_str!(…)",
        ),
        (
            "#[cfg_attr(all(), path = \"../other/mod.rs\")] mod sneaky;",
            0,
            "#[cfg_attr(…, path = …)]",
        ),
        (
            "use std::include as inc; fn f() { inc!(\"../x.rs\"); }",
            0,
            "use std::include",
        ),
        (
            "use core::{include_str as s, include_bytes};",
            0,
            "use core::include_str",
        ),
    ];
    for (source, depth, want) in cases {
        let got = violations(source, "me", depth);

        assert!(
            got.iter().any(|v| v == want),
            "{source:?}: got {got:?}, want {want}"
        );
    }
}

#[test]
fn check_slice_tracks_module_depth_through_files_and_dirs() {
    let dir = tempfile::TempDir::new().unwrap();
    let files = [
        ("mod.rs", "mod foo; use crate::app; use super::up;"),
        (
            "foo.rs",
            "mod bar; use super::helper; use super::super::up;",
        ),
        (
            "foo/bar.rs",
            "use super::super::helper; use super::super::super::up;",
        ),
        (
            "foo/baz/mod.rs",
            "use super::super::helper; use super::super::super::up;",
        ),
    ];
    for (path, source) in files {
        let path = dir.path().join(path);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(path, source).unwrap();
    }

    let mut found = check_slice(dir.path(), "me");

    let prefix = format!("{}/", dir.path().display());
    found
        .iter_mut()
        .for_each(|f| *f = f.replacen(&prefix, "", 1));
    found.sort();
    assert_eq!(
        found,
        [
            "foo.rs uses super::super::up",
            "foo/bar.rs uses super::super::super::up",
            "foo/baz/mod.rs uses super::super::super::up",
            "mod.rs uses super::up",
        ]
    );
}
