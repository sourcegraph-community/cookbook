//! Cache directory + generic helpers shared by every cache kind (file list,
//! recency, symbols, repo slug). Nothing here is Sourcegraph- or git-specific.

use std::collections::hash_map::DefaultHasher;
use std::hash::{Hash, Hasher};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

/// Root cache directory: `$XDG_CACHE_HOME/claude-file-suggestion` or
/// `~/.cache/claude-file-suggestion`. Created lazily by [`ensure_cache_dir`].
pub fn cache_dir() -> Option<PathBuf> {
    let base = std::env::var_os("XDG_CACHE_HOME")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".cache")))?;
    Some(base.join("claude-file-suggestion"))
}

/// `cache_dir()`, creating it if missing. Returns `None` on any I/O failure
/// so callers can degrade to "no cache" rather than panic (spec §2).
pub fn ensure_cache_dir() -> Option<PathBuf> {
    let dir = cache_dir()?;
    std::fs::create_dir_all(&dir).ok()?;
    Some(dir)
}

/// Stable, non-cryptographic hash of cache-key parts, joined with `|` before
/// hashing so `("a","bc")` and `("ab","c")` never collide. Used to turn
/// variable-length identifiers (paths, URLs) into short cache filenames.
pub fn hash_key(parts: &[&str]) -> String {
    let mut hasher = DefaultHasher::new();
    parts.join("|").hash(&mut hasher);
    format!("{:016x}", hasher.finish())
}

/// `SystemTime` -> nanoseconds-since-epoch, used to persist mtimes as plain
/// decimal text (avoids a time-formatting crate for a value that is only
/// ever round-tripped through comparison, never displayed).
fn system_time_nanos(t: SystemTime) -> u128 {
    t.duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0)
}

/// Read-through disk cache invalidated by a witness file's mtime: returns the
/// stored text when `witness` has not been touched since it was written,
/// otherwise calls `compute` and persists both the value and the new mtime.
///
/// `key_parts[0]` also names the cache file (`files-<hash>`, `repo-<hash>`) so
/// a stray cache entry is identifiable by eye. Any cache I/O failure degrades
/// to calling `compute` directly rather than failing the caller (spec §2).
pub fn cached_by_mtime<F>(key_parts: &[&str], witness: &Path, compute: F) -> Option<String>
where
    F: FnOnce() -> Option<String>,
{
    let paths = std::fs::metadata(witness)
        .ok()
        .and_then(|m| m.modified().ok())
        .zip(ensure_cache_dir())
        .map(|(mtime, dir)| {
            let key = hash_key(key_parts);
            let name = format!("{}-{key}", key_parts.first().copied().unwrap_or("c"));
            (dir.join(format!("{name}.meta")), dir.join(name), mtime)
        });

    if let Some((meta_path, data_path, mtime)) = &paths {
        let stored = std::fs::read_to_string(meta_path)
            .ok()
            .and_then(|m| m.trim().parse::<u128>().ok());
        if stored == Some(system_time_nanos(*mtime)) {
            if let Ok(contents) = std::fs::read_to_string(data_path) {
                return Some(contents);
            }
        }
    }

    let fresh = compute()?;
    if let Some((meta_path, data_path, mtime)) = &paths {
        let _ = std::fs::write(data_path, &fresh);
        let _ = std::fs::write(meta_path, system_time_nanos(*mtime).to_string());
    }
    Some(fresh)
}

/// Seconds since a path's mtime, or `None` if the path does not exist or its
/// mtime cannot be read. Used for the symbol cache TTL and stale-lock checks.
pub fn age_secs(path: &Path) -> Option<u64> {
    let modified = std::fs::metadata(path).ok()?.modified().ok()?;
    let elapsed = SystemTime::now().duration_since(modified).ok()?;
    Some(elapsed.as_secs())
}
