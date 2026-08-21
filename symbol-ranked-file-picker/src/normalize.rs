//! Query/name normalization shared by basename matching (§6.1) and symbol
//! matching (§6.2-6.4). Kept in one place so "what counts as the same name"
//! never drifts between the two call sites.

/// Fold case and strip `_`/`-` so `dropguard`, `drop_guard`, `DropGuard` and
/// `Drop-Guard` all normalize identically (§6.4: matching is always
/// case-insensitive, never inferred from the query's own casing).
///
/// **Do not** apply this to text sent to Sourcegraph (§6.3) — the wire
/// prefix must stay raw-lowercase so `py_r` (117 hits) isn't mangled into
/// `pyr` (0 hits).
pub fn normalize(s: &str) -> String {
    s.chars()
        .filter(|c| *c != '_' && *c != '-')
        .flat_map(char::to_lowercase)
        .collect()
}

/// A tracked path's basename with its extension *stripped* and normalize()
/// applied — the value used for the §6.6 definition-vs-re-export tiebreak.
///
/// Not for the §6.1 hoist: see [`basename_matches`], which has to reason about
/// the extension the query may carry. Comparing this against a normalized
/// query was the §6.1 bug — `normalize("os.rs")` keeps its dot, this drops it,
/// so the two could never be equal.
pub fn normalized_basename(path: &str) -> String {
    let file_name = path.rsplit('/').next().unwrap_or(path);
    let stem = file_name
        .rsplit_once('.')
        .map_or(file_name, |(stem, _)| stem);
    normalize(stem)
}

/// Split a basename into (normalized stem, lowercased extension). A
/// leading-dot name (`.gitignore`) or an empty extension has no extension.
fn basename_parts(file_name: &str) -> (String, Option<String>) {
    match file_name.rsplit_once('.') {
        Some((stem, ext)) if !stem.is_empty() && !ext.is_empty() => {
            (normalize(stem), Some(ext.to_lowercase()))
        }
        _ => (normalize(file_name), None),
    }
}

/// True when `query` names this path's basename, for the §6.1 exact-match
/// hoist. A query carrying an extension must match it, so `os.rs` hoists the
/// files actually named `os.rs` and not `os.md` or `os.pyi`; a bare `os` still
/// matches any extension, as it always did.
///
/// Queries are typed with the extension far more often than not, and every one
/// of them used to miss the hoist entirely and fall through to fuzzy path
/// scoring — `@os.rs` in monty returned `os_tests.rs` and `os_dispatch.rs`
/// while the two real `os.rs` files sat below them.
pub fn basename_matches(path: &str, query: &str) -> bool {
    let file_name = path.rsplit('/').next().unwrap_or(path);
    let (path_stem, path_ext) = basename_parts(file_name);
    let (query_stem, query_ext) = basename_parts(query);
    path_stem == query_stem && (query_ext.is_none() || query_ext == path_ext)
}

#[cfg(test)]
mod tests {
    use super::basename_matches;

    #[test]
    fn query_with_extension_matches_only_that_extension() {
        assert!(basename_matches("crates/monty-types/src/os.rs", "os.rs"));
        assert!(basename_matches("crates/monty/src/modules/os.rs", "os.rs"));
        assert!(!basename_matches("limitations/os.md", "os.rs"));
        assert!(!basename_matches("crates/monty-typeshed/custom/os.pyi", "os.rs"));
    }

    #[test]
    fn bare_query_still_matches_any_extension() {
        assert!(basename_matches("crates/monty/src/value.rs", "value"));
        assert!(basename_matches("crates/monty-js/ts/worker/value.ts", "value"));
        assert!(basename_matches("limitations/os.md", "os"));
    }

    #[test]
    fn near_misses_do_not_match() {
        assert!(!basename_matches("crates/monty/tests/os_tests.rs", "os.rs"));
        assert!(!basename_matches("crates/monty/src/os_dispatch.rs", "os.rs"));
    }

    #[test]
    fn separators_and_case_still_fold() {
        assert!(basename_matches("crates/monty/src/heap_traits.rs", "heaptraits"));
        assert!(basename_matches("crates/monty/src/heap_traits.rs", "HEAP-TRAITS.rs"));
    }

    #[test]
    fn leading_dot_names_have_no_extension() {
        assert!(basename_matches("some/dir/.gitignore", ".gitignore"));
    }
}
