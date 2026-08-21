//! Sourcegraph symbol lookup: local cache filtering on the hot path (§4c) and
//! the detached `--warm` fetch (§5) that populates it.
//!
//! The two halves share `cache_path` and `sym_query` so a hot-path miss and
//! the `--warm` process it spawns always agree on where the answer belongs.

use std::collections::{HashMap, HashSet};
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::time::Duration;

use crate::cache::{age_secs, ensure_cache_dir, hash_key};
use crate::normalize::{normalize, normalized_basename};

/// Cache/fetch granularity and gate (spec §4c, §6.2): fetch once per 4-char
/// prefix and filter locally for every longer query in the same word, so
/// `drop` -> `dropg` -> `dropgu` -> `dropguard` is one cold miss, not four.
pub const PREFIX_LEN: usize = 4;
const CACHE_TTL_SECS: u64 = 60 * 60 * 24;
const FETCH_COUNT: u32 = 500;
const FETCH_TIMEOUT: Duration = Duration::from_secs(25);
/// A fetch lock older than this was left behind by a killed or hung `--warm`
/// (spec §4c) and must not wedge the fallback forever. Comfortably above
/// `FETCH_TIMEOUT` so a slow-but-live fetch is never mistaken for a dead one.
const LOCK_STALE_SECS: u64 = 90;

/// The lowercased (never-normalized, §6.3) first `PREFIX_LEN` *characters* of
/// the query, or `None` if the query has fewer than that. Sliced by `char`,
/// not by byte: a byte slice at a fixed offset panics on non-ASCII input
/// (e.g. a query containing a multi-byte character straddling byte 4), which
/// would violate spec §2's "never panic" on nothing more exotic than a user
/// typing an accented or CJK filename.
pub fn prefix_of(query: &str) -> Option<String> {
    let prefix: String = query.chars().take(PREFIX_LEN).collect();
    (prefix.chars().count() == PREFIX_LEN).then(|| prefix.to_lowercase())
}

/// Does a fresh symbol-fetch lock exist at `path`? A `true` here means some
/// other process is already warming this prefix, so the caller (hot path or
/// `--warm` itself) must not start a second fetch. A missing or stale
/// ([`LOCK_STALE_SECS`]) lock is not "active".
fn active_lock(path: &Path) -> bool {
    age_secs(path).is_some_and(|age| age <= LOCK_STALE_SECS)
}

/// Where the symbol cache/lock for this `(endpoint, repo, prefix)` live.
pub fn cache_paths(endpoint: &str, repo: &str, prefix: &str) -> Option<(PathBuf, PathBuf)> {
    let dir = ensure_cache_dir()?;
    let key = hash_key(&["sym", endpoint, repo, prefix]);
    Some((
        dir.join(format!("sym-{key}")),
        dir.join(format!("sym-{key}.lock")),
    ))
}

/// A symbol name claimed by this many distinct tracked files or more carries
/// no useful evidence about *which* file the user meant. `string` is declared
/// in 9 files here and `resolve` in 10; every one of them tied at the exact-
/// match weight and `git ls-files` order picked the winner, so typing the
/// last letter of a common word made ranking worse than the prefix before it.
const COMMON_SYMBOL_FILES: usize = 5;

/// One symbol match tier, ordered so `Ord`/`max` picks the strongest (spec
/// §7: exact > prefix > substring, all of which must outrank nothing and the
/// prefix tier specifically must outrank a weak fuzzy path hit).
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug)]
pub enum Tier {
    Substring,
    Prefix,
    Exact,
}

impl Tier {
    /// This tier weakened by `steps` levels: exact reads as prefix, prefix as
    /// substring, substring as `None` ("no usable symbol evidence"). The two
    /// demotion sources — a ubiquitous symbol name (here) and a prose file
    /// (`score::is_prose`) — both go through this, so they compose: a common
    /// symbol mentioned in a markdown heading drops two tiers and scores 0.
    pub fn demote(self, steps: u32) -> Option<Tier> {
        match (self as u32).checked_sub(steps) {
            Some(2) => Some(Tier::Exact),
            Some(1) => Some(Tier::Prefix),
            Some(0) => Some(Tier::Substring),
            _ => None,
        }
    }
}

/// The strongest symbol match found for one file, plus the two facts scoring
/// needs about it: whether it is a *definition* rather than a re-export (§6.6:
/// a file whose basename contains the matched symbol name outranks one that
/// merely imports it), and whether the matched name is claimed by so many
/// files that it is noise (see [`COMMON_SYMBOL_FILES`]).
#[derive(Clone, Copy)]
pub struct SymbolMatch {
    pub tier: Tier,
    pub is_eponymous: bool,
    pub is_common: bool,
}

impl SymbolMatch {
    /// Ordering key for picking one file's best match out of several: the
    /// tier it will actually score at (post-rarity-demotion) first, then
    /// definition-over-re-export. Comparing *effective* tiers matters — a
    /// rare prefix match is better evidence than a ubiquitous exact one.
    fn strength(&self) -> (Option<Tier>, bool) {
        (
            self.tier.demote(u32::from(self.is_common)),
            self.is_eponymous,
        )
    }
}

/// Read the cached `normalized_symbol<TAB>path` lines for `prefix` (if fresh)
/// and classify every line against the full (already-normalized) query into
/// per-path best-tier matches. Returns `None` on a cold/stale/missing cache —
/// the caller then knows to self-spawn `--warm` instead of blocking.
///
/// Only paths present in `tracked` survive (spec §3: an `@` mention must
/// resolve to a file actually on disk).
pub fn lookup(
    endpoint: &str,
    repo: &str,
    prefix: &str,
    normalized_query: &str,
    tracked: &HashSet<&str>,
) -> Option<HashMap<String, SymbolMatch>> {
    let (cache_path, _lock_path) = cache_paths(endpoint, repo, prefix)?;
    if age_secs(&cache_path)? >= CACHE_TTL_SECS {
        return None;
    }
    let contents = std::fs::read_to_string(&cache_path).ok()?;
    Some(classify(&contents, normalized_query, tracked))
}

/// The ranking half of [`lookup`], split out from the cache read so the two
/// passes below are testable without a filesystem fixture: turn cache lines
/// into one best `SymbolMatch` per tracked path.
fn classify(
    contents: &str,
    normalized_query: &str,
    tracked: &HashSet<&str>,
) -> HashMap<String, SymbolMatch> {
    // First pass: classify, and count how many distinct files claim each
    // matched symbol name. The cache lines are sorted and deduped by
    // `fetch_and_write`, so one line is one (symbol, file) claim and the
    // count needs no further deduplication.
    let mut matches: Vec<(&str, &str, Tier)> = Vec::new();
    let mut claims: HashMap<&str, usize> = HashMap::new();
    for line in contents.lines() {
        let Some((symbol, path)) = line.split_once('\t') else {
            continue;
        };
        if !tracked.contains(path) {
            continue;
        }
        let tier = if symbol == normalized_query {
            Tier::Exact
        } else if symbol.starts_with(normalized_query) {
            Tier::Prefix
        } else if symbol.contains(normalized_query) {
            Tier::Substring
        } else {
            continue;
        };
        *claims.entry(symbol).or_default() += 1;
        matches.push((symbol, path, tier));
    }

    // Second pass: keep the strongest match per file, now that rarity is known.
    let mut best: HashMap<String, SymbolMatch> = HashMap::new();
    for (symbol, path, tier) in matches {
        let candidate = SymbolMatch {
            tier,
            is_eponymous: normalized_basename(path).contains(symbol),
            is_common: claims.get(symbol).copied().unwrap_or(0) >= COMMON_SYMBOL_FILES,
        };
        best.entry(path.to_string())
            .and_modify(|m| {
                if candidate.strength() > m.strength() {
                    m.tier = candidate.tier;
                    m.is_common = candidate.is_common;
                }
                m.is_eponymous |= candidate.is_eponymous;
            })
            .or_insert(candidate);
    }
    best
}

/// Should the hot path bother with the symbol channel at all? Per spec §5: no
/// path separator (`@src/foo` is a path hint, not a symbol query) and — §6.1 —
/// no exact basename match already found (which wins outright and is also
/// strictly faster). The "long enough to have a prefix" half of the gate is
/// [`prefix_of`] itself, which the caller applies first.
pub fn should_attempt(query: &str, has_basename_exact: bool) -> bool {
    !has_basename_exact && !query.contains('/')
}

/// Self-spawn `file-suggestion --warm <prefix>` fully detached: no waited-on
/// child, no inherited stdout/stdin (spec §5's "must not be in the response
/// path"). Best-effort — a spawn failure just means symbols stay cold for
/// this prefix a while longer, never a hang or an error on our own stdout.
pub fn spawn_warm(prefix: &str) {
    let Ok(exe) = std::env::current_exe() else {
        return;
    };
    let _ = std::process::Command::new(exe)
        .arg("--warm")
        .arg(prefix)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn();
}

/// Should the hot path trigger a fetch for this prefix? False when a fresh
/// symbol cache already answers it, or when another process's fetch lock is
/// still live — callers use this to decide whether to call [`spawn_warm`].
pub fn needs_warm(endpoint: &str, repo: &str, prefix: &str) -> bool {
    let Some((cache_path, lock_path)) = cache_paths(endpoint, repo, prefix) else {
        return false;
    };
    let cache_fresh = age_secs(&cache_path).is_some_and(|age| age < CACHE_TTL_SECS);
    !cache_fresh && !active_lock(&lock_path)
}

// --- `--warm <prefix>`: the only network-touching, only-spawned-detached path ---

/// Entry point for `file-suggestion --warm <prefix>`. Resolves the repo,
/// takes the fetch lock, queries Sourcegraph, and writes the (possibly
/// empty, i.e. negative-cached) symbol list. Never panics and never prints —
/// this process's stdout/stderr were redirected to `/dev/null` by the parent
/// anyway, but it may also be invoked directly for `make`-style debugging.
pub fn run_warm(prefix: &str, repo: &crate::git::Repo, endpoint: &str) {
    let Some(repo_slug) = crate::git::repo_slug(repo) else {
        return;
    };
    let Some((cache_path, lock_path)) = cache_paths(endpoint, &repo_slug, prefix) else {
        return;
    };

    if active_lock(&lock_path) {
        return; // another warm for this exact prefix is already in flight
    }
    if std::fs::File::create(&lock_path).is_err() {
        return;
    }
    let _ = fetch_and_write(&repo_slug, prefix, endpoint, &cache_path);
    let _ = std::fs::remove_file(&lock_path);
}

/// GraphQL response shape we care about — everything else is ignored so a
/// Sourcegraph schema addition can never break decoding (`serde`'s default
/// behavior already ignores unknown fields; these structs just say so).
#[derive(serde::Deserialize)]
struct GraphQlResponse {
    data: Option<SearchData>,
}
#[derive(serde::Deserialize)]
struct SearchData {
    search: Option<SearchResult>,
}
#[derive(serde::Deserialize)]
struct SearchResult {
    results: Option<ResultsWrapper>,
}
#[derive(serde::Deserialize)]
struct ResultsWrapper {
    results: Vec<FileMatch>,
}
#[derive(serde::Deserialize, Default)]
struct FileMatch {
    file: Option<FileInfo>,
    #[serde(default)]
    symbols: Vec<SymbolInfo>,
}
#[derive(serde::Deserialize)]
struct FileInfo {
    path: String,
}
#[derive(serde::Deserialize)]
struct SymbolInfo {
    name: String,
}

/// Escape `.` for interpolation into the `repo:^...$` regex filter — a raw
/// `.` would match any character, silently widening the repo filter.
fn escape_repo_regex(repo: &str) -> String {
    repo.replace('.', "\\.")
}

/// POST the symbol search to Sourcegraph and write `normalized<TAB>path`
/// lines (sorted, deduped) to `cache_path`. An empty result is written too —
/// caching a true negative is what stops a dead prefix from re-paying the
/// round trip on every keystroke (spec §4c).
fn fetch_and_write(
    repo_slug: &str,
    prefix: &str,
    endpoint: &str,
    cache_path: &PathBuf,
) -> Option<()> {
    let sg_query = format!(
        "repo:^{}$ type:symbol {prefix} count:{FETCH_COUNT}",
        escape_repo_regex(repo_slug)
    );
    let body = serde_json::json!({
        "query": "query($q:String!){search(query:$q,version:V3){results{results{__typename ... on FileMatch{file{path} symbols{name}}}}}}",
        "variables": {"q": sg_query},
    });

    let url = format!("{endpoint}/.api/graphql");
    let agent = ureq::Agent::config_builder()
        .timeout_global(Some(FETCH_TIMEOUT))
        .build()
        .new_agent();
    let mut request = agent
        .post(url.as_str())
        .header("Content-Type", "application/json");

    // Security invariant (spec §5): an instance token may only ride to the
    // instance it was issued for. The bash version attached it unconditionally
    // and leaked a demo-instance token to sourcegraph.com; both conditions
    // below are mandatory, not either/or.
    if let (Ok(token), Ok(src_endpoint)) = (
        std::env::var("SRC_ACCESS_TOKEN"),
        std::env::var("SRC_ENDPOINT"),
    ) {
        if src_endpoint == endpoint {
            request = request.header("Authorization", &format!("token {token}"));
        }
    }

    let mut response = request.send_json(&body).ok()?;
    let parsed: GraphQlResponse = response.body_mut().read_json().ok()?;

    let mut lines: Vec<String> = parsed
        .data
        .and_then(|d| d.search)
        .and_then(|s| s.results)
        .map(|r| r.results)
        .unwrap_or_default()
        .into_iter()
        .flat_map(|file_match| {
            let path = file_match.file.map(|f| f.path);
            file_match.symbols.into_iter().filter_map(move |sym| {
                path.clone()
                    .map(|p| format!("{}\t{p}", normalize(&sym.name)))
            })
        })
        .collect();
    lines.sort();
    lines.dedup();

    let mut tmp = cache_path.clone();
    tmp.set_extension("tmp");
    let mut file = std::fs::File::create(&tmp).ok()?;
    file.write_all(lines.join("\n").as_bytes()).ok()?;
    drop(file);
    std::fs::rename(&tmp, cache_path).ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tracked<'a>(list: &[&'a str]) -> HashSet<&'a str> {
        list.iter().copied().collect()
    }

    #[test]
    fn prefix_is_char_sliced_and_lowercased() {
        assert_eq!(prefix_of("DropGuard").as_deref(), Some("drop"));
        assert_eq!(prefix_of("dro"), None);
        // Spec §2: a byte slice at a fixed offset would panic here.
        assert_eq!(prefix_of("héllo").as_deref(), Some("héll"));
        assert_eq!(prefix_of("日本語"), None);
    }

    #[test]
    fn demotions_stack_down_to_nothing() {
        assert_eq!(Tier::Exact.demote(0), Some(Tier::Exact));
        assert_eq!(Tier::Exact.demote(1), Some(Tier::Prefix));
        assert_eq!(Tier::Exact.demote(2), Some(Tier::Substring));
        assert_eq!(Tier::Exact.demote(3), None);
        assert_eq!(Tier::Substring.demote(1), None);
    }

    #[test]
    fn strongest_tier_per_file_wins() {
        let cache = "dropguardian\tsrc/guard.rs\ndropguard\tsrc/guard.rs\n";
        let out = classify(cache, "dropguard", &tracked(&["src/guard.rs"]));
        assert_eq!(out["src/guard.rs"].tier, Tier::Exact);
    }

    #[test]
    fn untracked_paths_and_non_matches_are_dropped() {
        // Spec §3: an `@` mention must resolve to a file actually on disk.
        let cache = "dropguard\tvendor/gone.rs\nunrelated\tsrc/guard.rs\n";
        let out = classify(cache, "dropguard", &tracked(&["src/guard.rs"]));
        assert!(out.is_empty());
    }

    #[test]
    fn eponymous_marks_the_definition_site() {
        let cache = "dropguard\tsrc/drop_guard.rs\ndropguard\tsrc/lib.rs\n";
        let out = classify(
            cache,
            "dropguard",
            &tracked(&["src/drop_guard.rs", "src/lib.rs"]),
        );
        assert!(out["src/drop_guard.rs"].is_eponymous);
        assert!(!out["src/lib.rs"].is_eponymous);
    }

    #[test]
    fn a_name_claimed_by_many_files_is_marked_common() {
        let paths: Vec<String> = (0..COMMON_SYMBOL_FILES)
            .map(|i| format!("src/f{i}.rs"))
            .collect();
        let refs: Vec<&str> = paths.iter().map(String::as_str).collect();

        let at_threshold: String = refs.iter().map(|p| format!("string\t{p}\n")).collect();
        let out = classify(&at_threshold, "string", &tracked(&refs));
        assert_eq!(out.len(), COMMON_SYMBOL_FILES);
        assert!(out.values().all(|m| m.is_common));

        let below: String = refs[1..].iter().map(|p| format!("string\t{p}\n")).collect();
        let out = classify(&below, "string", &tracked(&refs));
        assert!(out.values().all(|m| !m.is_common));
    }
}
