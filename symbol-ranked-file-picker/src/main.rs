//! `claude-file-suggestion`: Claude Code's `fileSuggestion` hook, rewritten
//! from a working-but-185ms-per-keystroke bash script (`~/.claude/file-suggestion.sh`)
//! for latency (p95 < 15ms warm — see spec/README). The ranking logic and
//! every landmine fix it preserves are documented inline near where they're
//! implemented; caveats live in `README.md`.
//!
//! Two entry points:
//! - default (stdin hook mode): read `{"query": "..."}`, print up to 15
//!   repo-relative paths, best first, exit 0 always (spec §2).
//! - `--warm <prefix>`: perform the Sourcegraph fetch for `<prefix>` and
//!   write the symbol cache. The *only* subprocess the hot path may create,
//!   and it is spawned fully detached (spec §5) — nothing here may ever
//!   block the hook on the network.

mod cache;
mod debug_log;
mod git;
mod normalize;
mod score;
mod symbols;

use std::collections::HashSet;
use std::io::Read as _;
use std::time::Instant;

use normalize::normalize;

const SUGGESTION_LIMIT: usize = 15;

fn main() {
    // Spec §2 is absolute: "never panic". Every fallible step below already
    // degrades internally rather than propagating an error, but a hook that
    // is spawned on every keystroke is exactly the wrong place to trust that
    // no future change (or edge case, e.g. a byte-slice on non-ASCII input)
    // ever introduces one — a visible panic is a broken keystroke for the
    // user. This is a safety net, not a substitute for fixing panics: any
    // `catch_unwind` here should be treated as a bug report, not a feature.
    std::panic::set_hook(Box::new(|_| {}));
    let _ = std::panic::catch_unwind(|| {
        let args: Vec<String> = std::env::args().collect();
        match args.get(1).map(String::as_str) {
            Some("--warm") => run_warm_mode(args.get(2).map(String::as_str)),
            _ => run_hook_mode(),
        }
    });
    std::process::exit(0);
}

/// `--warm <prefix>`: resolve the repo and hand off to `symbols::run_warm`.
/// Prints nothing on any path — this runs detached with stdio redirected to
/// `/dev/null` by the parent, so there is nothing downstream to read it.
fn run_warm_mode(prefix: Option<&str>) {
    let Some(prefix) = prefix else { return };
    let Ok(cwd) = std::env::current_dir() else {
        return;
    };
    let project_dir = project_dir(&cwd);
    let Some(repo) = git::discover_repo(&project_dir) else {
        return;
    };
    let endpoint = sg_endpoint();
    symbols::run_warm(prefix, &repo, &endpoint);
}

/// The `fileSuggestion` hook itself: stdin -> ranked paths on stdout.
fn run_hook_mode() {
    let start = Instant::now();

    let Some(query) = read_query() else { return };
    if query.is_empty() {
        return;
    }

    let project_dir = project_dir(&std::env::current_dir().unwrap_or_default());
    // Spec §2: not inside a git work tree => exit 0 silently. This is a
    // filesystem walk (git::discover_repo), not a `git` spawn.
    let Some(repo) = git::discover_repo(&project_dir) else {
        return;
    };

    let tracked = git::tracked_files(&repo);
    if tracked.is_empty() {
        return;
    }
    let recent = git::recent_files(&repo);

    let normalized_query = normalize(&query);
    let tracked_set: HashSet<&str> = tracked.iter().map(String::as_str).collect();

    // §6.1: an exact basename match wins outright and skips the symbol path
    // entirely — computed here (not inside score::rank) because it also
    // gates whether we touch Sourcegraph at all.
    let has_basename_exact = tracked
        .iter()
        .any(|p| normalize::normalized_basename(p) == normalized_query);

    let endpoint = sg_endpoint();
    let symbol_matches = if symbols::should_attempt(&query, has_basename_exact) {
        let prefix = symbols::prefix_of(&query).expect("should_attempt implies a prefix exists");
        match git::repo_slug(&repo) {
            Some(repo_slug) => match symbols::lookup(
                &endpoint,
                &repo_slug,
                &prefix,
                &normalized_query,
                &tracked_set,
            ) {
                Some(matches) => matches,
                None => {
                    // Cold/stale cache: never block on the network (spec §5).
                    // Emit local-only results now and let a detached `--warm`
                    // fill the cache in for the *next* keystroke.
                    if symbols::needs_warm(&endpoint, &repo_slug, &prefix) {
                        symbols::spawn_warm(&prefix);
                    }
                    Default::default()
                }
            },
            None => Default::default(),
        }
    } else {
        Default::default()
    };

    let ranked = score::rank(
        &tracked,
        &normalized_query,
        &query,
        &symbol_matches,
        &recent,
        SUGGESTION_LIMIT,
    );

    let mut out = String::new();
    for r in &ranked {
        out.push_str(r.path);
        out.push('\n');
    }
    print!("{out}");

    log_debug(&query, &symbol_matches, &ranked, start);
}

/// Read `{"query": "..."}` from stdin. Malformed JSON, a missing field, or a
/// closed/empty stdin are all treated the same as "no query" (spec §2) —
/// this must never be the thing that makes the hook visibly fail.
fn read_query() -> Option<String> {
    let mut buf = String::new();
    std::io::stdin().read_to_string(&mut buf).ok()?;
    if buf.trim().is_empty() {
        return Some(String::new());
    }
    #[derive(serde::Deserialize)]
    struct Input {
        #[serde(default)]
        query: String,
    }
    let input: Input = serde_json::from_str(&buf).ok()?;
    Some(input.query)
}

/// `$CLAUDE_PROJECT_DIR` if set, else the process's current directory (spec §2).
fn project_dir(fallback_cwd: &std::path::Path) -> std::path::PathBuf {
    std::env::var_os("CLAUDE_PROJECT_DIR")
        .map(std::path::PathBuf::from)
        .unwrap_or_else(|| fallback_cwd.to_path_buf())
}

/// `$CLAUDE_SG_ENDPOINT` or the public default (spec §5).
fn sg_endpoint() -> String {
    std::env::var("CLAUDE_SG_ENDPOINT").unwrap_or_else(|_| "https://sourcegraph.com".to_string())
}

/// Spec §9 debug line, only written when `CLAUDE_FILE_SUGGESTION_DEBUG` is set.
fn log_debug(
    query: &str,
    symbol_matches: &std::collections::HashMap<String, symbols::SymbolMatch>,
    ranked: &[score::Scored<'_>],
    start: Instant,
) {
    if std::env::var_os("CLAUDE_FILE_SUGGESTION_DEBUG").is_none() {
        return;
    }
    let exact = symbol_matches
        .values()
        .filter(|m| m.tier == symbols::Tier::Exact)
        .count();
    let partial = symbol_matches
        .values()
        .filter(|m| m.tier != symbols::Tier::Exact)
        .count();
    let local = ranked.iter().filter(|r| r.fuzzy_hit).count();
    let top = ranked.first().map(|r| r.path);
    debug_log::log(
        query,
        exact,
        local,
        partial,
        start.elapsed().as_millis(),
        top,
    );
}
