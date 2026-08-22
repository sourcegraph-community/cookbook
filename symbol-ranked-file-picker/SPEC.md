# Spec: `claude-file-suggestion` — Rust rewrite

Build a single Rust binary that serves Claude Code's `fileSuggestion` hook. It
replaces an existing bash script that works correctly but is too slow to use.

**Read this whole document before writing code.** Sections 6 and 7 encode bugs
that were already found and fixed the hard way in the bash version. A
from-scratch implementation reliably reintroduces them.

This is the build spec the shipped code cites by section number (`§6.1`,
`§10`, ...). Where spec and code disagree, the code in this directory wins and
the spec is the bug. `test_harness.py` is the executable half of §10.

---

## 1. Why this rewrite exists

The bash version is at `~/.claude/file-suggestion.sh`. Its *logic* is correct
and this spec preserves it. Its *latency* is fatal: measured mean **185ms per
keystroke** across 21 real invocations, never below 118ms.

Cause is process spawn count, not algorithm. Measured spawn floor on the target
machine (macOS, Apple silicon):

| spawn | cost |
|---|---|
| `git ls-files` (1127 files) | 30ms |
| `fzf --filter` | 32ms |
| `jq` | 20ms |
| `date` | 18ms |
| `cksum` | 17ms |

The script spawns ~14 processes → 265ms measured end to end. No amount of bash
tuning reaches a usable budget.

**Hard latency requirement: p95 under 15ms warm, under 25ms cold.** This is the
single most important acceptance criterion. If a design choice trades accuracy
for latency, take the latency. The picker runs on every keystroke.

---

## 2. Interface

Invoked by Claude Code as a hook, one process per keystroke.

- **stdin**: one JSON object, `{"query": "dropguard"}`. Ignore unknown fields.
  Absent/empty query → exit 0, print nothing.
- **stdout**: repo-relative paths, newline separated, best first, **max 15**.
- **exit code**: always 0. Never panic, never print to stdout on error. A
  broken suggestion provider must degrade to "no suggestions", never to a
  visible error or a hang.
- **cwd**: `$CLAUDE_PROJECT_DIR` if set, else the process cwd. If it is not
  inside a git work tree, exit 0 silently.

Also implement a second, non-hook mode:

- `--warm <prefix>` — perform the Sourcegraph fetch for `<prefix>` and write the
  symbol cache. Prints nothing. Used by the hot path to self-spawn detached
  (§5). This is the only subprocess the hot path may create.

Install location: `~/.claude/file-suggestion` (binary). Cargo project lives in
this directory. Standalone crate, **not** part of any host workspace — it must
build against any repo.

---

## 3. What it does

Given a query, return the 15 best file paths from the current git repo, ranked
by a blend of:

1. **Filename match** — fuzzy, like the built-in picker.
2. **Symbol match** — via Sourcegraph, so `resolve_path` finds
   `path_security.rs` and `collect_cycles` finds `heap.rs`, files whose names
   contain none of the query.
3. **Git recency** — a file touched in the last 25 commits beats an
   equally-scoring file that was not.

Only paths that exist in `git ls-files` may be emitted. An `@` mention must
resolve to a file on this disk; a Sourcegraph result for a path not checked out
locally is dropped.

---

## 4. Caches

Four, all under `${XDG_CACHE_HOME:-~/.cache}/claude-file-suggestion/`.

### 4a. File list (new — the bash version lacked this)

`git ls-files` costs 30ms and must leave the hot path.

- Cache the newline-separated tracked-file list to disk.
- **Invalidate on `.git/index` mtime.** Store that mtime alongside the list;
  compare on each run; re-shell to `git ls-files` only when it differs.
- Reading the cached list must be a plain file read (~1ms), no spawn.

### 4b. Git recency

- `git log --pretty=format: --name-only -n 25` → deduped file list.
- Keyed on `PWD | HEAD sha`. HEAD changing is the only thing that invalidates it.
- `RECENT_COMMITS = 25` and **do not raise it**. At 300 commits this repo marks
  948/1127 files (84%) as "recent", which is not a signal. At 25 it marks 316
  (28%), which reranks visibly.

### 4c. Symbols

- Format: one `normalized_symbol_name<TAB>repo_relative_path` per line, sorted,
  deduped.
- **Keyed on a 4-character query prefix**, not the full query. See §6.2. The
  full key is `(endpoint, repo slug, prefix)`, so pointing
  `CLAUDE_SG_ENDPOINT` at a different instance does not read another
  instance's results.
- TTL 24h. Cache negative results too — a prefix with no symbols must not
  re-pay the round trip on every keystroke.
- Lock file next to the cache to dedupe concurrent warms; treat a lock older
  than 90s as stale and ignore it (`symbols::LOCK_STALE_SECS`).

### 4d. Repo slug

- `git config --get remote.origin.url`, normalized to `host/owner/repo` (§5).
- Invalidated on `.git/config` mtime. Deriving it costs a spawn, and the answer
  changes about never.

---

## 5. Sourcegraph fetch

Only when: query length ≥ 4, query contains no `/`, and no exact basename match
already exists (§6.1).

Endpoint `${CLAUDE_SG_ENDPOINT:-https://sourcegraph.com}`, POST
`/.api/graphql`. Works unauthenticated for public repos — do not require a token.

Repo is derived from `git config --get remote.origin.url`, normalizing both
`git@host:owner/repo.git` and `https://host/owner/repo.git` to `host/owner/repo`.
Escape `.` when interpolating into the `repo:^...$` regex.

Search query:

```
repo:^{escaped_repo}$ type:symbol {prefix} count:500
```

GraphQL:

```graphql
query($q:String!){search(query:$q,version:V3){results{results{
  __typename ... on FileMatch{file{path} symbols{name}}}}}}
```

### Never block on the network

The fetch **must not** be in the response path. On a cache miss: emit
local-only results immediately, and self-spawn `--warm <prefix>` fully
detached (do not wait, do not read its output, do not let it hold stdout).
Symbols appear one keystroke later. Fetch timeout 25s — it is generous
precisely because nothing is waiting on it.

The hot path must also skip the self-spawn when another process already holds a
fresh fetch lock (`symbols::needs_warm`). Without that check, every keystroke
of a long query spawns its own duplicate `--warm` for the same prefix.

### Token handling — security requirement

```
attach Authorization header  ⟺  SRC_ACCESS_TOKEN is set
                             AND SRC_ENDPOINT == the endpoint being called
```

The bash version originally attached `SRC_ACCESS_TOKEN` unconditionally and
leaked a `demo.sourcegraph.com` token to `sourcegraph.com`. The token had to be
rotated. **Both conditions are mandatory.** Never send an instance credential to
a host merely because it is the default.

---

## 6. Known landmines — every one of these was a real, shipped bug

Treat each as a hard requirement with a regression test.

### 6.1 Exact basename must win outright

`@heap` means `heap.rs`. It must never mean a `heap` field that Sourcegraph
indexed in `heap_traits.rs`.

When a tracked file's basename (extension stripped, `_`/`-` removed,
lowercased) equals the normalized query, hoist it to the top **and skip the
symbol path entirely**. This is also the latency win for common short queries.

Normalize the query once per process, not once per candidate path
(`normalize::BasenameQuery`). This runs against every tracked file on every
keystroke, so re-deriving it inside the loop is measurable at 1127 files.

### 6.2 Cache on the prefix, not the query

Keying the symbol cache on the full query meant `drop` → `dropg` → `dropgu` →
`dropguard` were four cold misses in a row. The cache never paid off mid-word,
which is the only time it matters.

Fetch once per 4-char prefix, then filter locally for every longer query.

### 6.3 Send the raw prefix on the wire, normalized only locally

Sourcegraph matches raw symbol text. Normalizing before the request is fatal:

```
prefix "pyr"   → 0 symbols     (normalized — wrong)
prefix "py_r"  → 117 symbols   (raw lowercase — correct)
```

Lowercase the prefix for the request. **Do not strip `_` or `-` from it.**
Normalization (case + separator folding) applies to the *local comparison
only*, so a typed `dropguard` still matches the symbol `DropGuard`.

### 6.4 Matching is always case-insensitive

fzf's smart-case silently made `MountTable` and `mounttable` behave differently
— uppercase returned 0 hits while lowercase returned 5 with the right file
first. This made the bash version look far better in testing than in use.

Fold case unconditionally. Never infer case sensitivity from the query.

### 6.5 No result-count gates

The bash version skipped the symbol path when `local_count < 5`. The flagship
query `mounttable` scored exactly 5 junk fuzzy hits, so `5 < 5` was false and
Sourcegraph was never called. The feature could not have worked.

Do not gate on hit counts. Gate only on §5's stated conditions.

### 6.6 Definition beats re-export

`mounttable` ranked `lib.rs` (a re-export) above `mount_table.rs` (the
definition). When two files both hold an exact symbol match, the one whose
basename contains the symbol name ranks higher.

---

## 7. Ranking — the outstanding bug, and the main behavioral change

The bash version concatenated four lists in fixed order:

```
basename_exact → exact_symbol → local_fuzzy → partial_symbol
```

`partial_symbol` last is wrong, and it is why the user still sees the tool as
broken. From the real debug log, typing `dropguard`:

```
q=dropg    exact=0  local=6  partial=2
q=dropgu   exact=0  local=1  partial=2
q=dropgua  exact=0  local=0  partial=2
```

The symbol match was present on **every** keystroke from `dropg` onward. It was
correct. But at `dropg` it sat at rank 7-8, buried under six fuzzy filename
hits, so it was never seen. Lookup works; ranking hides it.

**Replace the four lists with one scored candidate set.** Every tracked path
gets a score; sort once; emit the top 15. Suggested components, tune with real
queries:

| signal | effect | shipped weight (`score::weight`) |
|---|---|---|
| basename exact (normalized) | dominant — outranks everything | 1_000_000 |
| symbol name exact | large | 100_000 |
| symbol name prefix (query is a prefix of the symbol) | **medium — must beat a weak fuzzy path hit** | 5_000 |
| symbol name substring | small | 800 |
| fuzzy path score | base | `nucleo-matcher` score, a few hundred at most |
| touched in last 25 commits | tiebreak boost | 50 |
| definition in eponymous file (§6.6) | tiebreak boost | 200 |

The gaps between tiers are deliberately wide: a tier is meant to dominate the
one below it outright, not to be summed past by a good fuzzy score.

The mid-word partial-symbol case is the one this rewrite exists to fix. Verify
it explicitly: at `dropg`, `dropgu`, and `dropgua`, the `DropGuard` file must
land in the **top 3**.

### 7.1 A symbol tier is evidence only when the name is rare

Symbol tiers above are not absolute: a name claimed by many files says nothing
about *which* file the user meant. `string` is declared in 9 files in the
monty repo and `resolve` in 10; all tied at the exact weight and `git ls-files`
order picked the winner, so ranking got **worse** as the user finished a word:

```
q=strin    top=crates/monty/src/string_builder.rs   good
q=string   top=crates/monty-js/src/telemetry.rs     bad  (9 files claim `string`)
```

Two conditions demote a symbol match **one full tier** — exact reads as prefix,
prefix as substring, substring as nothing — and they stack:

| condition | why |
|---|---|
| the matched name is claimed by ≥5 tracked files (`symbols::COMMON_SYMBOL_FILES`) | ubiquitous identifier, no discriminating power |
| the file is prose (`.md`, `.stderr`, ...) | Sourcegraph indexes headings and captured compiler output like declarations |

Demote rather than drop: weak evidence still beats none when nothing else
matches. The general principle is that when symbol evidence is weak, filename
agreement decides — `string` then resolves to `string_builder.rs`, which has a
rare prefix match, a strong fuzzy path hit and the eponymous bonus.

---

## 8. Implementation notes

- Edition 2021+. Keep the dependency set small; every dep is startup cost.
- Fuzzy matching: use `nucleo-matcher` (the matcher behind Helix) rather than
  hand-rolling. Configure it case-insensitive (§6.4).
- HTTP: something light with rustls, e.g. `ureq`. Only used in `--warm`, so its
  cost never touches the hot path — but do not let it bloat the binary's
  startup.
- JSON: `serde_json`.
- No `unwrap()` on anything derived from I/O, stdin, or the network. Every
  failure path degrades to fewer results, never a panic (§2).
- Style: expression-oriented; `if`/`match` as expressions with a tail value
  rather than early `return` guards, except where several guards genuinely open
  a function.
- Docstrings on every struct, enum, and public fn: what it does *and why*,
  plus foot-guns. Concise — a few lines. Comments where logic is non-obvious,
  especially each §6 landmine, citing the failure it prevents.

---

## 9. Debug logging

Preserve it — it is how this thing gets diagnosed against real usage, and
standalone testing has already proven a poor substitute.

When `CLAUDE_FILE_SUGGESTION_DEBUG` is set, append one line per invocation to
`<cache_dir>/debug.log`:

```
HH:MM:SS q=<query> exact=<n> local=<n> partial=<n> ms=<n>
```

Add `top=<first result path>` to the line. The old format showed counts but not
what was actually emitted, which made the ranking bug (§7) much harder to see
than it should have been.

---

## 10. Acceptance

Build a test harness that feeds queries on stdin and checks rank + latency, so
these are reproducible rather than eyeballed.

**Latency** (repo: pydantic/monty, 1127 tracked files)

- warm cache, p95 < 15ms
- cold cache, p95 < 25ms
- no invocation may block on the network, ever

**Ranking** — expected file in the top 3. These assert against symbol names in
pydantic/monty as that repo carries them, so a case can go red because upstream
renamed something rather than because ranking broke. `RANKING_CASES` in
`test_harness.py` is the live list; keep the two in sync.

| query | expected | why the case exists |
|---|---|---|
| `heap_traits` | `crates/monty/src/heap_traits.rs` | §6.1 basename exact |
| `heap_traits.rs` | same | §6.1 with the extension typed |
| `all.rs` | `crates/monty/src/builtins/all.rs` | §6.1, and the case that actually fails without the hoist |
| `dropg` | `crates/monty/src/heap_traits.rs` (`DropGuard`) | **§7, the mid-word case this rewrite exists for** |
| `dropgu` | same | same |
| `dropgua` | same | same |
| `dropguard` | same | same |
| `resolve_virtual_path` | `crates/monty-fs/src/path_security.rs` | symbol-only: the query appears nowhere in the path |
| `collect_cycles` | `crates/monty/src/heap/mod.rs` | symbol-only |
| `string` | `crates/monty/src/string_builder.rs` | §7.1, 9 files claim `string` |
| `resolve` | `crates/monty-fs/src/path_security.rs` | §7.1, 10 files claim `resolve` |
| `mounttable` | `crates/monty-fs/src/mount_table.rs` | not `lib.rs`, §6.6 |
| `MountTable` | identical results to `mounttable` | §6.4 |
| `py_repr_fmt` | reported, not asserted | §6.3 — verify the wire prefix is `py_r`; no single defining file to pin |

Upstream drift already hit this table once: `heap` was the original §6.1 case
until monty refactored `heap.rs` into `heap/mod.rs`, leaving no file whose
basename stem is `heap`, and `resolve_path` was renamed
`resolve_virtual_path`. Assert the name the index actually carries.

**Known limitation, do not chase:** `macro_rules!` macros are absent from
Sourcegraph's symbol index, so a query like `defer_drop` will not resolve
through the symbol channel. This is upstream, not a bug in this tool.

**Robustness (§2)** — exit 0 and print nothing on: empty stdin, malformed JSON,
missing `query` field, empty query, a multi-byte query, and a cwd that is not
inside a git work tree. The multi-byte case is a real bug this harness caught:
`symbols::prefix_of` byte-sliced the query at a fixed offset, which panics
(exit 101) the moment a character's bytes straddle it.

**Security**

- With `SRC_ENDPOINT=https://demo.sourcegraph.com` and `SRC_ACCESS_TOKEN` set,
  a request to `https://sourcegraph.com` carries **no** Authorization header.
  Test this.

---

## 11. Deliverables

1. The cargo project in this directory, release-built.
2. Binary installed at `~/.claude/file-suggestion`.
3. `test_harness.py` + the §10 results, reported as measured numbers.
4. `README.md`: what it does, install, config env vars, and the §10 limitation.

**Do not modify `~/.claude/settings.json` on the reader's behalf.** Cutting over
is their call, after they have seen measured results. Leave whatever
`fileSuggestion` hook they already have in place as a fallback.
