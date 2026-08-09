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

/// A tracked path's basename with its extension and normalize() applied —
/// the value compared against the normalized query for the §6.1 exact-match
/// hoist and the §6.6 definition-vs-re-export tiebreak.
pub fn normalized_basename(path: &str) -> String {
    let file_name = path.rsplit('/').next().unwrap_or(path);
    let stem = file_name
        .rsplit_once('.')
        .map_or(file_name, |(stem, _)| stem);
    normalize(stem)
}
