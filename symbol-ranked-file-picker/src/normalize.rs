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

/// Split a trailing extension off a basename. A leading-dot name
/// (`.gitignore`) or an empty extension has no extension part.
fn split_ext(file_name: &str) -> (&str, Option<&str>) {
    match file_name.rsplit_once('.') {
        Some((stem, ext)) if !stem.is_empty() && !ext.is_empty() => (stem, Some(ext)),
        _ => (file_name, None),
    }
}

/// A query prepared once for §6.1 basename matching. Built outside the
/// per-candidate loop on purpose: this runs against every tracked file on
/// every keystroke, so normalizing the query 1,100 times over would show up
/// in the warm p95 the whole design is built around.
pub struct BasenameQuery {
    stem: String,
    ext: Option<String>,
}

impl BasenameQuery {
    /// Prepare `query` for [`BasenameQuery::matches`].
    pub fn new(query: &str) -> BasenameQuery {
        let (stem, ext) = split_ext(query);
        BasenameQuery {
            stem: normalize(stem),
            ext: ext.map(str::to_lowercase),
        }
    }

    /// True when the query names this path's basename, for the §6.1 hoist. A
    /// query carrying an extension must match it, so `os.rs` hoists the files
    /// actually named `os.rs` and not `os.md` or `os.pyi`; a bare `os` still
    /// matches any extension, as it always did.
    ///
    /// Queries are typed with the extension more often than not, and every one
    /// of them used to miss the hoist entirely and fall through to fuzzy path
    /// scoring: `normalize("os.rs")` keeps its dot, `normalized_basename`
    /// drops it, so the two could never be equal.
    pub fn matches(&self, path: &str) -> bool {
        let file_name = path.rsplit('/').next().unwrap_or(path);
        let (stem, ext) = split_ext(file_name);
        // Extension first: a byte compare that rejects most candidates before
        // the stem normalization has to allocate anything.
        if let Some(want) = &self.ext {
            match ext {
                Some(have) if have.eq_ignore_ascii_case(want) => {}
                _ => return false,
            }
        }
        normalize(stem) == self.stem
    }
}

#[cfg(test)]
mod tests {
    use super::BasenameQuery;

    fn matches(path: &str, query: &str) -> bool {
        BasenameQuery::new(query).matches(path)
    }

    #[test]
    fn query_with_extension_matches_only_that_extension() {
        assert!(matches("crates/monty-types/src/os.rs", "os.rs"));
        assert!(matches("crates/monty/src/modules/os.rs", "os.rs"));
        assert!(!matches("limitations/os.md", "os.rs"));
        assert!(!matches("crates/monty-typeshed/custom/os.pyi", "os.rs"));
    }

    #[test]
    fn bare_query_still_matches_any_extension() {
        assert!(matches("crates/monty/src/value.rs", "value"));
        assert!(matches("crates/monty-js/ts/worker/value.ts", "value"));
        assert!(matches("limitations/os.md", "os"));
    }

    #[test]
    fn near_misses_do_not_match() {
        assert!(!matches("crates/monty/tests/os_tests.rs", "os.rs"));
        assert!(!matches("crates/monty/src/os_dispatch.rs", "os.rs"));
    }

    #[test]
    fn separators_and_case_still_fold() {
        assert!(matches("crates/monty/src/heap_traits.rs", "heaptraits"));
        assert!(matches("crates/monty/src/heap_traits.rs", "HEAP-TRAITS.rs"));
    }

    #[test]
    fn leading_dot_names_have_no_extension() {
        assert!(matches("some/dir/.gitignore", ".gitignore"));
    }
}
