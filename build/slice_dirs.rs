//! Which directories under `src/slices/` are slices. `build.rs` and the
//! tests both include this file, so they can't disagree.

use std::path::Path;

/// Returns the names of the slice directories directly under `root`, sorted.
///
/// Every directory is a slice, except those whose name starts with `.` or
/// `_`. A slice directory must hold a `mod.rs` and its name must be a
/// snake_case module name; otherwise this fails, naming the directory.
pub fn slice_dirs(root: &Path) -> Result<Vec<String>, String> {
    let entries =
        std::fs::read_dir(root).map_err(|err| format!("read {}: {err}", root.display()))?;
    let mut names = Vec::new();
    for entry in entries {
        let entry = entry.map_err(|err| format!("read {}: {err}", root.display()))?;
        let path = entry.path();
        let name = entry.file_name().to_string_lossy().into_owned();
        if !path.is_dir() || name.starts_with('.') || name.starts_with('_') {
            continue;
        }
        if !is_module_name(&name) {
            return Err(format!(
                "slice directory {} must have a snake_case module name",
                path.display()
            ));
        }
        if KEYWORDS.contains(&name.as_str()) {
            return Err(format!(
                "slice directory {} is named after the Rust keyword {name:?}, which can't be a module name",
                path.display()
            ));
        }
        if !path.join("mod.rs").is_file() {
            return Err(format!(
                "slice directory {} has no mod.rs; every slice is a module directory",
                path.display()
            ));
        }
        names.push(name);
    }
    names.sort();
    Ok(names)
}

/// The lowercase Rust keywords, strict and reserved, as of edition 2024.
const KEYWORDS: &[&str] = &[
    "abstract", "as", "async", "await", "become", "box", "break", "const", "continue", "crate",
    "do", "dyn", "else", "enum", "extern", "false", "final", "fn", "for", "gen", "if", "impl",
    "in", "let", "loop", "macro", "match", "mod", "move", "mut", "override", "priv", "pub", "ref",
    "return", "self", "static", "struct", "super", "trait", "true", "try", "type", "typeof",
    "unsafe", "unsized", "use", "virtual", "where", "while", "yield",
];

fn is_module_name(name: &str) -> bool {
    let mut chars = name.chars();
    chars.next().is_some_and(|c| c.is_ascii_lowercase())
        && chars.all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_')
}
