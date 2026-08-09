//! Git state without spawning `git` on the hot path.
//!
//! The bash version paid for a `git` (or `git ls-files`/`git log`) spawn on
//! every keystroke; each spawn costs ~15-30ms on this machine, which alone
//! blows the 15ms warm budget. Everything here reads `.git/*` files directly
//! and only falls back to spawning `git` for genuinely rare shapes (packed
//! refs we can't parse, unusual configs) — and even then the result is
//! cached so the fallback pays its cost at most once.

use std::path::{Path, PathBuf};
use std::process::Command;

use crate::cache::{age_secs, ensure_cache_dir, hash_key, system_time_nanos};

/// A resolved git repository: the work tree root, the (possibly
/// worktree-specific) git dir holding `HEAD`/`index`, and the common git dir
/// holding `config`/`packed-refs` (same as `git_dir` outside of worktrees).
pub struct Repo {
    pub work_dir: PathBuf,
    pub git_dir: PathBuf,
    pub common_dir: PathBuf,
}

/// Walk up from `start` looking for a `.git` entry. Returns `None` if none is
/// found before the filesystem root, which per spec §2 means "exit 0
/// silently" — this is a plain `stat` walk, not a `git` spawn.
pub fn discover_repo(start: &Path) -> Option<Repo> {
    let mut dir = start.canonicalize().unwrap_or_else(|_| start.to_path_buf());
    loop {
        let dot_git = dir.join(".git");
        if let Some(repo) = read_dot_git(&dir, &dot_git) {
            return Some(repo);
        }
        if !dir.pop() {
            return None;
        }
    }
}

/// Interpret a `.git` entry as either a real git dir (normal repo) or a
/// gitfile (`gitdir: <path>`, used by worktrees and submodules).
fn read_dot_git(work_dir: &Path, dot_git: &Path) -> Option<Repo> {
    let meta = std::fs::symlink_metadata(dot_git).ok()?;
    let git_dir = if meta.is_dir() {
        dot_git.to_path_buf()
    } else {
        let contents = std::fs::read_to_string(dot_git).ok()?;
        let pointee = contents.trim().strip_prefix("gitdir:")?.trim();
        let pointee_path = PathBuf::from(pointee);
        if pointee_path.is_absolute() {
            pointee_path
        } else {
            work_dir.join(pointee_path)
        }
    };
    let common_dir = resolve_common_dir(&git_dir);
    Some(Repo {
        work_dir: work_dir.to_path_buf(),
        git_dir,
        common_dir,
    })
}

/// Worktrees keep `HEAD`/`index` in their own git dir but share `config` and
/// refs with the main repo via a `commondir` file. Non-worktree repos have no
/// such file, so `common_dir` just equals `git_dir`.
fn resolve_common_dir(git_dir: &Path) -> PathBuf {
    match std::fs::read_to_string(git_dir.join("commondir")) {
        Ok(contents) => {
            let rel = PathBuf::from(contents.trim());
            if rel.is_absolute() {
                rel
            } else {
                git_dir.join(rel)
            }
        }
        Err(_) => git_dir.to_path_buf(),
    }
}

/// Tracked files (`git ls-files`), cached on disk and invalidated only when
/// `.git/index`'s mtime changes (spec §4a). The common case — index
/// unchanged since last run — is a single file read, no spawn.
pub fn tracked_files(repo: &Repo) -> Vec<String> {
    let index_path = repo.git_dir.join("index");
    let index_mtime = std::fs::metadata(&index_path)
        .ok()
        .and_then(|m| m.modified().ok());

    let cache_dir = ensure_cache_dir();
    let key = hash_key(&["files", &repo.work_dir.to_string_lossy()]);
    let list_path = cache_dir.as_ref().map(|d| d.join(format!("files-{key}")));
    let meta_path = cache_dir
        .as_ref()
        .map(|d| d.join(format!("files-{key}.meta")));

    if let (Some(list_path), Some(meta_path)) = (&list_path, &meta_path) {
        if let (Ok(cached_meta), Some(mtime)) = (std::fs::read_to_string(meta_path), index_mtime) {
            if cached_meta.trim().parse::<u128>().ok() == Some(system_time_nanos(mtime)) {
                if let Ok(contents) = std::fs::read_to_string(list_path) {
                    return contents.lines().map(str::to_owned).collect();
                }
            }
        }
    }

    // Cache miss (or no cache dir available): pay for the one spawn this
    // module allows, then persist so the next keystroke doesn't repeat it.
    let files = run_git(&repo.work_dir, &["ls-files"])
        .map(|out| out.lines().map(str::to_owned).collect::<Vec<_>>())
        .unwrap_or_default();

    if let (Some(list_path), Some(meta_path), Some(mtime)) = (&list_path, &meta_path, index_mtime) {
        let _ = std::fs::write(list_path, files.join("\n"));
        let _ = std::fs::write(meta_path, system_time_nanos(mtime).to_string());
    }
    files
}

/// Files touched in the last `RECENT_COMMITS` commits, deduped, cached per
/// `(work_dir, HEAD sha)` (spec §4b). Keep `RECENT_COMMITS` small — at 300
/// commits this repo marks 84% of files "recent", which stops being a signal.
pub const RECENT_COMMITS: u32 = 25;

pub fn recent_files(repo: &Repo) -> std::collections::HashSet<String> {
    let head_sha = resolve_head_sha(repo).unwrap_or_else(|| "none".to_string());
    let cache_dir = ensure_cache_dir();
    let key = hash_key(&["recent", &repo.work_dir.to_string_lossy(), &head_sha]);
    let path = cache_dir.map(|d| d.join(format!("recent-{key}")));

    if let Some(path) = &path {
        if let Ok(contents) = std::fs::read_to_string(path) {
            return contents.lines().map(str::to_owned).collect();
        }
    }

    let arg = format!("-n{RECENT_COMMITS}");
    let files: std::collections::HashSet<String> = run_git(
        &repo.work_dir,
        &["log", "--pretty=format:", "--name-only", &arg],
    )
    .map(|out| {
        out.lines()
            .filter(|l| !l.is_empty())
            .map(str::to_owned)
            .collect()
    })
    .unwrap_or_default();

    if let Some(path) = &path {
        let joined: Vec<&str> = files.iter().map(String::as_str).collect();
        let _ = std::fs::write(path, joined.join("\n"));
    }
    files
}

/// Resolve `HEAD` to a commit sha by reading `.git/HEAD` and following the
/// ref chain by hand (loose ref file, then `packed-refs`). Falls back to
/// spawning `git rev-parse HEAD` only if both lookups fail — unusual repo
/// states (e.g. a freshly-init'd repo with no commits) rather than the
/// common path.
fn resolve_head_sha(repo: &Repo) -> Option<String> {
    let head = std::fs::read_to_string(repo.git_dir.join("HEAD")).ok()?;
    let head = head.trim();
    let Some(ref_name) = head.strip_prefix("ref:") else {
        return Some(head.to_string()); // detached HEAD: file holds the sha directly
    };
    let ref_name = ref_name.trim();

    if let Ok(sha) = std::fs::read_to_string(repo.common_dir.join(ref_name)) {
        return Some(sha.trim().to_string());
    }
    if let Ok(packed) = std::fs::read_to_string(repo.common_dir.join("packed-refs")) {
        for line in packed.lines() {
            if line.starts_with('#') {
                continue;
            }
            let mut parts = line.split_whitespace();
            if let (Some(sha), Some(r)) = (parts.next(), parts.next()) {
                if r == ref_name {
                    return Some(sha.to_string());
                }
            }
        }
    }
    run_git(&repo.work_dir, &["rev-parse", "HEAD"]).map(|s| s.trim().to_string())
}

/// The `host/owner/repo` slug used as the Sourcegraph repo filter, derived
/// from `remote.origin.url` and cached against `config`'s mtime — deriving it
/// needs a `git config` spawn only the first time (or after the remote
/// changes), same pattern as [`tracked_files`].
pub fn repo_slug(repo: &Repo) -> Option<String> {
    let config_path = repo.common_dir.join("config");
    let config_mtime = std::fs::metadata(&config_path)
        .ok()
        .and_then(|m| m.modified().ok());

    let cache_dir = ensure_cache_dir();
    let key = hash_key(&["repo", &repo.common_dir.to_string_lossy()]);
    let slug_path = cache_dir.as_ref().map(|d| d.join(format!("repo-{key}")));
    let meta_path = cache_dir
        .as_ref()
        .map(|d| d.join(format!("repo-{key}.meta")));

    if let (Some(slug_path), Some(meta_path)) = (&slug_path, &meta_path) {
        if let (Ok(cached_meta), Some(mtime)) = (std::fs::read_to_string(meta_path), config_mtime) {
            if cached_meta.trim().parse::<u128>().ok() == Some(system_time_nanos(mtime)) {
                if let Ok(slug) = std::fs::read_to_string(slug_path) {
                    return Some(slug.trim().to_string()).filter(|s| !s.is_empty());
                }
            }
        }
    }

    let url = read_remote_origin_url(&config_path)
        .or_else(|| run_git(&repo.work_dir, &["config", "--get", "remote.origin.url"]))?;
    let slug = normalize_repo_url(url.trim());

    if let (Some(slug_path), Some(meta_path), Some(mtime)) = (&slug_path, &meta_path, config_mtime)
    {
        let _ = std::fs::write(slug_path, &slug);
        let _ = std::fs::write(meta_path, system_time_nanos(mtime).to_string());
    }
    Some(slug).filter(|s| !s.is_empty())
}

/// Minimal INI read of `remote.origin.url` from a git `config` file. Only
/// handles the common unquoted/`[section "sub"]` shape; anything exotic
/// (includes, escaped quotes) falls through to the `git config` spawn above.
fn read_remote_origin_url(config_path: &Path) -> Option<String> {
    let contents = std::fs::read_to_string(config_path).ok()?;
    let mut in_origin = false;
    for raw_line in contents.lines() {
        let line = raw_line.trim();
        if let Some(section) = line.strip_prefix('[').and_then(|s| s.strip_suffix(']')) {
            in_origin = section.eq_ignore_ascii_case(r#"remote "origin""#);
            continue;
        }
        if in_origin {
            if let Some((key, value)) = line.split_once('=') {
                if key.trim().eq_ignore_ascii_case("url") {
                    return Some(value.trim().to_string());
                }
            }
        }
    }
    None
}

/// Normalize `git@host:owner/repo.git` and `https://host/owner/repo.git`
/// (with or without userinfo) to `host/owner/repo`, matching the bash
/// version's `sed` pipeline exactly so cache keys don't shift on rewrite.
fn normalize_repo_url(url: &str) -> String {
    let mut s = url.to_string();

    // `git@host:path` -> `host/path` (only when the `@` precedes the `:` and
    // nothing before it looks like a path already, i.e. it's the scp-style
    // shorthand rather than a URL with userinfo).
    if let (Some(at_idx), Some(colon_idx)) = (s.find('@'), s.find(':')) {
        if at_idx < colon_idx && !s[..at_idx].contains('/') {
            let host = s[at_idx + 1..colon_idx].to_string();
            let path = s[colon_idx + 1..].to_string();
            s = format!("{host}/{path}");
        }
    }
    if let Some(idx) = s.find("://") {
        s = s[idx + 3..].to_string();
    }
    if let Some(idx) = s.find('@') {
        s = s[idx + 1..].to_string();
    }
    if let Some(stripped) = s.strip_suffix(".git") {
        s = stripped.to_string();
    }
    s
}

/// Spawn `git <args>` in `dir`, returning trimmed stdout on success. The only
/// process-spawning escape hatch in this module — used solely for the rare
/// cache-miss/fallback paths documented above, never unconditionally.
fn run_git(dir: &Path, args: &[&str]) -> Option<String> {
    let output = Command::new("git")
        .args(args)
        .current_dir(dir)
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    String::from_utf8(output.stdout)
        .ok()
        .map(|s| s.trim_end().to_string())
}

/// Does a fresh (<=90s old) symbol-fetch lock exist at `path`? A `true` here
/// means some other process is already warming this prefix, so the caller
/// (hot path or `--warm` itself) must not start a second fetch. A missing or
/// stale (spec §4c) lock is not "active" — a stale one was left behind by a
/// killed/hung fetch and must not wedge the fallback forever.
pub fn active_lock(path: &Path) -> bool {
    age_secs(path).is_some_and(|age| age <= 90)
}
