//! The single scored candidate set (spec §7). Every tracked path gets one
//! score; sort once; emit the top N. Replaces the bash version's four
//! concatenated lists, whose fixed order buried a correct partial-symbol
//! match at rank 7-8 on every keystroke from `dropg` onward.

use std::collections::{HashMap, HashSet};

use nucleo_matcher::pattern::{CaseMatching, Normalization, Pattern};
use nucleo_matcher::{Config, Matcher};

use crate::normalize::BasenameQuery;
use crate::symbols::{SymbolMatch, Tier};

/// Score weights, ordered exactly as spec §7's table. Each tier must clear
/// the one below it by more than that tier's own internal spread (fuzzy
/// score, eponymous bonus) can ever close — see the comment on each constant.
mod weight {
    /// Dominant: an exact basename match outranks everything else,
    /// full stop (§6.1 — `@heap` must never lose to a `heap` field
    /// Sourcegraph indexed elsewhere).
    pub const BASENAME_EXACT: i64 = 1_000_000;
    /// Large: the query names a symbol exactly — but only when that name is
    /// rare enough to identify a file (`symbols::COMMON_SYMBOL_FILES`);
    /// otherwise it is demoted to the tier below by `tier_weight`.
    pub const SYMBOL_EXACT: i64 = 100_000;
    /// Medium, but must beat a weak fuzzy path hit (nucleo scores for a
    /// short query against a long path rarely exceed a few hundred) — this
    /// is the fix for §7's `dropg`/`dropgu`/`dropgua` mid-word case.
    pub const SYMBOL_PREFIX: i64 = 5_000;
    /// Small: still comfortably above typical fuzzy noise, but a prefix
    /// match should out-rank a plain substring one.
    pub const SYMBOL_SUBSTRING: i64 = 800;
    /// Tiebreak: added on top of whichever symbol tier matched, when the
    /// match is a definition rather than a re-export (§6.6).
    pub const EPONYMOUS_DEFINITION: i64 = 200;
    /// Tiebreak: a file touched in the last N commits beats an
    /// equally-scored one that wasn't (spec §4b/§7).
    pub const RECENT: i64 = 50;
}

/// Extensions whose "symbols" are prose or captured tool output, not
/// definitions. Sourcegraph indexes markdown headings and the identifiers
/// inside `.stderr` compile-fail fixtures exactly like real declarations, so
/// without this `heapreader` returned four `.stderr` files and no `heap.rs`,
/// and `dropguard` put `CLAUDE.md` above the type's actual definition.
const PROSE_EXTENSIONS: &[&str] = &[
    "md", "markdown", "rst", "txt", "stderr", "stdout", "log", "lock", "snap",
];

/// True when a symbol match in this path is a mention rather than a
/// definition. Extension-based on purpose: cheap (this runs per candidate on
/// the keystroke path) and predictable.
fn is_prose(path: &str) -> bool {
    path.rsplit_once('.')
        .is_some_and(|(_, ext)| PROSE_EXTENSIONS.iter().any(|p| p.eq_ignore_ascii_case(ext)))
}

/// Weight for a symbol match after `demotions` tiers of weakening (see
/// [`Tier::demote`]). Two things demote, and they stack: the file is prose,
/// and the matched symbol name is claimed by many files. Demoting rather than
/// dropping keeps weak evidence available when nothing else matches, while
/// guaranteeing a rarer or code-file match at the same tier outranks it.
fn tier_weight(tier: Tier, demotions: u32) -> i64 {
    match tier.demote(demotions) {
        Some(Tier::Exact) => weight::SYMBOL_EXACT,
        Some(Tier::Prefix) => weight::SYMBOL_PREFIX,
        Some(Tier::Substring) => weight::SYMBOL_SUBSTRING,
        None => 0,
    }
}

/// One ranked suggestion. `fuzzy_hit` is kept around post-sort only for
/// debug logging (§9's `local=` count).
pub struct Scored<'a> {
    pub path: &'a str,
    pub score: i64,
    pub fuzzy_hit: bool,
}

/// Score and rank every tracked file against `query`, returning the top
/// `limit` paths best-first. `bq` is the §6.1 basename query the caller
/// already built (it needs the answer before deciding whether to touch
/// Sourcegraph, so building a second one here would re-normalize every
/// tracked basename a second time on every keystroke). `symbols` is the
/// already-tier-classified match map from [`crate::symbols::lookup`] (or
/// empty if the symbol channel was skipped/cold this keystroke); `recent` is
/// the last-25-commits file set.
pub fn rank<'a>(
    tracked: &'a [String],
    query: &str,
    bq: &BasenameQuery,
    symbols: &HashMap<String, SymbolMatch>,
    recent: &HashSet<String>,
    limit: usize,
) -> Vec<Scored<'a>> {
    let mut matcher = Matcher::new(Config::DEFAULT.match_paths());
    let fuzzy: HashMap<&str, u32> = if query.is_empty() {
        HashMap::new()
    } else {
        Pattern::parse(query, CaseMatching::Ignore, Normalization::Smart)
            .match_list(tracked.iter().map(String::as_str), &mut matcher)
            .into_iter()
            .collect()
    };

    let mut candidates: Vec<Scored<'a>> = tracked
        .iter()
        .filter_map(|path| {
            let path = path.as_str();
            let is_basename_exact = bq.matches(path);
            let symbol = symbols.get(path);
            let fuzzy_score = fuzzy.get(path).copied();

            if !is_basename_exact && symbol.is_none() && fuzzy_score.is_none() {
                return None; // no signal at all: not a candidate (§7 — recency alone never qualifies)
            }

            let mut score: i64 = 0;
            if is_basename_exact {
                score += weight::BASENAME_EXACT;
            }
            if let Some(m) = symbol {
                let prose = is_prose(path);
                score += tier_weight(m.tier, u32::from(prose) + u32::from(m.is_common));
                // The eponymous bonus marks a definition site, which a prose
                // mention never is — withhold it there.
                if m.is_eponymous && !prose {
                    score += weight::EPONYMOUS_DEFINITION;
                }
            }
            score += fuzzy_score.unwrap_or(0) as i64;
            if recent.contains(path) {
                score += weight::RECENT;
            }

            Some(Scored {
                path,
                score,
                fuzzy_hit: fuzzy_score.is_some(),
            })
        })
        .collect();

    // Stable sort on score alone: ties keep `git ls-files`' order, which is
    // plain alphabetical — deterministic and good enough, nothing in spec
    // asks for a further tiebreak.
    candidates.sort_by_key(|c| std::cmp::Reverse(c.score));
    candidates.truncate(limit);
    candidates
}

#[cfg(test)]
mod tests {
    use super::*;

    fn paths(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| s.to_string()).collect()
    }

    fn sym(tier: Tier, is_eponymous: bool, is_common: bool) -> SymbolMatch {
        SymbolMatch {
            tier,
            is_eponymous,
            is_common,
        }
    }

    /// `(path, score)` best-first. Fixtures deliberately use queries that
    /// fuzzy-match none of their paths, so scores are the weight table alone
    /// and ties fall back to input (`git ls-files`) order.
    fn ranked(
        tracked: &[String],
        query: &str,
        symbols: &[(&str, SymbolMatch)],
    ) -> Vec<(String, i64)> {
        let symbols: HashMap<String, SymbolMatch> =
            symbols.iter().map(|(p, m)| (p.to_string(), *m)).collect();
        rank(
            tracked,
            query,
            &BasenameQuery::new(query),
            &symbols,
            &HashSet::new(),
            SUGGESTION_LIMIT_FOR_TESTS,
        )
        .into_iter()
        .map(|s| (s.path.to_string(), s.score))
        .collect()
    }

    const SUGGESTION_LIMIT_FOR_TESTS: usize = 15;

    #[test]
    fn basename_exact_outranks_a_symbol_exact_elsewhere() {
        let tracked = paths(&["src/dropguard.rs", "src/registry.rs"]);
        let out = ranked(
            &tracked,
            "dropguard.rs",
            &[("src/registry.rs", sym(Tier::Exact, false, false))],
        );
        assert_eq!(out[0].0, "src/dropguard.rs");
    }

    #[test]
    fn prose_files_are_demoted_below_code() {
        let tracked = paths(&["docs/notes.md", "src/guard.rs"]);
        let out = ranked(
            &tracked,
            "dropguard",
            &[
                ("docs/notes.md", sym(Tier::Exact, false, false)),
                ("src/guard.rs", sym(Tier::Exact, false, false)),
            ],
        );
        assert_eq!(out[0].0, "src/guard.rs");
        assert_eq!(out[0].1, weight::SYMBOL_EXACT);
        assert_eq!(out[1].1, weight::SYMBOL_PREFIX);
    }

    #[test]
    fn definition_outranks_reexport_but_never_in_prose() {
        // Same tier: the eponymous (definition) file wins by the §6.6 bonus.
        let code = paths(&["a/zzz.rs", "b/yyy.rs"]);
        let out = ranked(
            &code,
            "dropguard",
            &[
                ("a/zzz.rs", sym(Tier::Prefix, false, false)),
                ("b/yyy.rs", sym(Tier::Prefix, true, false)),
            ],
        );
        assert_eq!(out[0].0, "b/yyy.rs");
        assert_eq!(
            out[0].1,
            weight::SYMBOL_PREFIX + weight::EPONYMOUS_DEFINITION
        );

        // Prose is never a definition site, so the bonus is withheld and the
        // tie falls back to `git ls-files` order.
        let prose = paths(&["a/zzz.md", "b/yyy.md"]);
        let out = ranked(
            &prose,
            "dropguard",
            &[
                ("a/zzz.md", sym(Tier::Prefix, false, false)),
                ("b/yyy.md", sym(Tier::Prefix, true, false)),
            ],
        );
        assert_eq!(out[0].0, "a/zzz.md");
        assert_eq!(out[0].1, out[1].1);
    }

    #[test]
    fn a_ubiquitous_symbol_name_loses_to_a_rare_one() {
        let tracked = paths(&["a/zzz.rs", "b/yyy.rs"]);
        let out = ranked(
            &tracked,
            "dropguard",
            &[
                ("a/zzz.rs", sym(Tier::Exact, false, true)),
                ("b/yyy.rs", sym(Tier::Exact, false, false)),
            ],
        );
        assert_eq!(out[0].0, "b/yyy.rs");
        assert_eq!(out[1].1, weight::SYMBOL_PREFIX);
    }

    #[test]
    fn prose_and_ubiquity_demotions_stack() {
        let tracked = paths(&["a/zzz.md"]);
        let out = ranked(
            &tracked,
            "dropguard",
            &[("a/zzz.md", sym(Tier::Exact, true, true))],
        );
        assert_eq!(out[0].1, weight::SYMBOL_SUBSTRING);
    }

    #[test]
    fn recency_alone_never_qualifies() {
        let tracked = paths(&["z/unrelated.txt"]);
        let recent: HashSet<String> = tracked.iter().cloned().collect();
        let out = rank(
            &tracked,
            "dropguard",
            &BasenameQuery::new("dropguard"),
            &HashMap::new(),
            &recent,
            SUGGESTION_LIMIT_FOR_TESTS,
        );
        assert!(out.is_empty());
    }

    #[test]
    fn recency_breaks_a_tie_between_equal_scores() {
        let tracked = paths(&["a/zzz.rs", "b/yyy.rs"]);
        let recent: HashSet<String> = ["b/yyy.rs".to_string()].into_iter().collect();
        let symbols: HashMap<String, SymbolMatch> = [
            ("a/zzz.rs".to_string(), sym(Tier::Prefix, false, false)),
            ("b/yyy.rs".to_string(), sym(Tier::Prefix, false, false)),
        ]
        .into_iter()
        .collect();
        let out = rank(
            &tracked,
            "dropguard",
            &BasenameQuery::new("dropguard"),
            &symbols,
            &recent,
            SUGGESTION_LIMIT_FOR_TESTS,
        );
        assert_eq!(out[0].path, "b/yyy.rs");
        assert_eq!(out[0].score - out[1].score, weight::RECENT);
    }
}
