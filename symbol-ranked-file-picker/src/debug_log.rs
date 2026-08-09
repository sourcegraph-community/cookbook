//! Opt-in per-invocation diagnostics (spec §9). Preserved from the bash
//! version because standalone testing has already proven a poor substitute
//! for seeing what a real keystroke stream actually returned — the §7
//! ranking bug was invisible in counts alone, which is why `top=` was added.

use std::io::Write as _;

/// Append one debug line to `<cache_dir>/debug.log` when
/// `CLAUDE_FILE_SUGGESTION_DEBUG` is set. Silently does nothing otherwise, and
/// silently drops any I/O error — debug logging must never be the reason a
/// keystroke fails (spec §2).
pub fn log(query: &str, exact: usize, local: usize, partial: usize, ms: u128, top: Option<&str>) {
    if std::env::var_os("CLAUDE_FILE_SUGGESTION_DEBUG").is_none() {
        return;
    }
    let Some(dir) = crate::cache::ensure_cache_dir() else {
        return;
    };
    let Ok(mut file) = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(dir.join("debug.log"))
    else {
        return;
    };
    // Wall-clock UTC, not local time like the bash version's `date +%H:%M:%S`
    // — a deliberate trade to avoid a timezone-database dependency on a
    // debug-only path; see README.
    let _ = writeln!(
        file,
        "{} q={:<22} exact={:<3} local={:<4} partial={:<3} ms={} top={}",
        utc_hh_mm_ss(),
        query,
        exact,
        local,
        partial,
        ms,
        top.unwrap_or(""),
    );
}

/// `HH:MM:SS` in UTC, computed by hand to avoid a time-formatting dependency
/// on a path that only ever runs when explicitly asked to.
fn utc_hh_mm_ss() -> String {
    let secs_of_day = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
        % 86_400;
    format!(
        "{:02}:{:02}:{:02}",
        secs_of_day / 3600,
        (secs_of_day % 3600) / 60,
        secs_of_day % 60
    )
}
