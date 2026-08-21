#!/usr/bin/env python3
"""Spec §10 acceptance harness for `file-suggestion`.

Feeds queries on stdin exactly as the real hook does (one process per
keystroke) and checks rank + latency, so results are reproducible rather
than eyeballed. Run against the pydantic/monty checkout:

    python3 test_harness.py /path/to/monty

By default it exercises the installed hook at `~/.claude/file-suggestion`.
Set `FILE_SUGGESTION_BIN` to test a build without installing it:

    FILE_SUGGESTION_BIN=target/release/file-suggestion \
        python3 test_harness.py /path/to/monty

The ranking cases assert against symbol names in pydantic/monty as your
Sourcegraph instance has them indexed, so a case can go red because upstream
renamed something rather than because ranking broke.

Sections:
  - ranking:  each query's actual top-3, with pass/fail against spec §10.
  - latency:  warm/cold p50/p95 over real subprocess spawns.
  - security: SRC_ACCESS_TOKEN must only ride to SRC_ENDPOINT, verified
              against a local HTTP server (never the real sourcegraph.com).
"""

import http.server
import json
import os
import shutil
import statistics
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

BINARY = Path(
    os.environ.get("FILE_SUGGESTION_BIN", Path.home() / ".claude" / "file-suggestion")
)
CACHE_DIR = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "claude-file-suggestion"


def clear_cache():
    shutil.rmtree(CACHE_DIR, ignore_errors=True)


def invoke(repo_dir, query, env_extra=None):
    """Run the binary exactly like the hook: JSON on stdin, paths on stdout."""
    env = dict(os.environ)
    env["CLAUDE_PROJECT_DIR"] = str(repo_dir)
    env.pop("CLAUDE_FILE_SUGGESTION_DEBUG", None)
    if env_extra:
        env.update(env_extra)
    start = time.perf_counter()
    result = subprocess.run(
        [str(BINARY)],
        input=json.dumps({"query": query}),
        capture_output=True,
        text=True,
        env=env,
        timeout=5,
    )
    elapsed_ms = (time.perf_counter() - start) * 1000
    paths = [line for line in result.stdout.splitlines() if line]
    return paths, elapsed_ms, result.returncode


# --- Ranking (spec §10) -------------------------------------------------

RANKING_CASES = [
    # §6.1 basename-exact, both ways the user types it. `heap` used to live
    # here, but monty refactored `heap.rs` into `heap/mod.rs` and no file's
    # basename stem is `heap` any more, so the case stopped exercising the
    # hoist it was written for.
    ("heap_traits", "crates/monty/src/heap_traits.rs"),
    # Same file, query typed with its extension. Fuzzy scoring alone happens
    # to rank `heap_traits.rs` first either way, so `all.rs` below is the case
    # that actually fails without the hoist.
    ("heap_traits.rs", "crates/monty/src/heap_traits.rs"),
    ("all.rs", "crates/monty/src/builtins/all.rs"),
    ("dropg", "crates/monty/src/heap_traits.rs"),  # DropGuard, §7 mid-word case
    ("dropgu", "crates/monty/src/heap_traits.rs"),
    ("dropgua", "crates/monty/src/heap_traits.rs"),
    ("dropguard", "crates/monty/src/heap_traits.rs"),
    # The symbol-channel case: the query appears nowhere in the filename, so
    # only a symbol match can find this file. Was `resolve_path` until monty
    # renamed it; assert the name the index actually carries, not the one a
    # local checkout happens to be pinned to.
    ("resolve_virtual_path", "crates/monty-fs/src/path_security.rs"),
    ("collect_cycles", "crates/monty/src/heap/mod.rs"),
    # §7.1 rarity demotion: `string` is claimed by 9 files here and `resolve`
    # by 10, so completing either word used to hand rank 1 to whatever
    # `git ls-files` listed first (telemetry.rs, worker/transport.ts) rather
    # than the file whose name also agrees with the query.
    ("string", "crates/monty/src/string_builder.rs"),
    ("resolve", "crates/monty-fs/src/path_security.rs"),
    ("mounttable", "crates/monty-fs/src/mount_table.rs"),  # not lib.rs, §6.6
    ("MountTable", "crates/monty-fs/src/mount_table.rs"),  # must match `mounttable`, §6.4
    ("py_repr_fmt", None),  # see note below: no single "the" defining file
]


def run_ranking(repo_dir):
    print("\n=== Ranking (spec §10) ===")
    clear_cache()
    # Prime the symbol cache for every 4-char prefix we're about to query,
    # then wait for the detached `--warm` fetches to land — otherwise the
    # first keystroke of each query is a legitimate cold miss (spec §5) and
    # the test would just be measuring the network, not the ranking.
    prefixes = sorted({q[:4].lower() for q, _ in RANKING_CASES if len(q) >= 4})
    for prefix in prefixes:
        subprocess.run(
            [str(BINARY), "--warm", prefix],
            env={**os.environ, "CLAUDE_PROJECT_DIR": str(repo_dir)},
            timeout=30,
        )

    all_pass = True
    mounttable_result = None
    for query, expected in RANKING_CASES:
        paths, ms, _ = invoke(repo_dir, query)
        top3 = paths[:3]
        if query == "mounttable":
            mounttable_result = top3
        if query == "MountTable":
            case_ok = top3 == mounttable_result
            status = "PASS" if case_ok else "FAIL"
            all_pass &= case_ok
            print(f"  [{status}] q={query!r:14} top3={top3}  (must equal 'mounttable' result)")
            continue
        if expected is None:
            print(f"  [INFO] q={query!r:14} top3={top3}  (no single defining file, see README)")
            continue
        case_ok = expected in top3
        status = "PASS" if case_ok else "FAIL"
        all_pass &= case_ok
        print(f"  [{status}] q={query!r:14} top3={top3}")
    return all_pass


# --- Regression: the six shipped-and-fixed-once bugs (spec §6) ------------


def _warm_and_wait(repo_dir, prefix, env_extra=None):
    env = {**os.environ, "CLAUDE_PROJECT_DIR": str(repo_dir)}
    if env_extra:
        env.update(env_extra)
    subprocess.run([str(BINARY), "--warm", prefix], env=env, timeout=30)


def run_regressions(repo_dir):
    print("\n=== Regression: spec §6 landmines ===")
    results = {}

    # §6.1 — exact basename must win outright, not just make the top 3. The
    # query carries its extension on purpose: that is the form that used to
    # miss the hoist entirely (see normalize::basename_matches). `all.rs` is
    # chosen because fuzzy scoring alone ranks `lib.rs` and `call.rs` above it,
    # so this assertion fails if the hoist ever stops firing.
    clear_cache()
    paths, _, _ = invoke(repo_dir, "all.rs")
    results["6.1 basename-exact is rank 1"] = (
        bool(paths) and paths[0] == "crates/monty/src/builtins/all.rs"
    )

    # §6.2 — cache on the 4-char prefix, not the full query: warming "drop"
    # once must answer dropg/dropgu/dropgua/dropguard without any further
    # fetch, i.e. exactly one sym-* cache file for the whole word.
    clear_cache()
    _warm_and_wait(repo_dir, "drop")
    for q in ("dropg", "dropgu", "dropgua", "dropguard"):
        invoke(repo_dir, q)
    sym_files = [p for p in CACHE_DIR.glob("sym-*") if not p.name.endswith(".lock")]
    results["6.2 one cache file serves the whole word"] = len(sym_files) == 1

    # §6.3 — the wire prefix must be raw-lowercase, not normalized: `py_r`
    # (117 hits) vs a normalized `pyr` (0 hits, verified separately via
    # direct GraphQL call against the live API). If the cache from warming
    # "py_r" contains a `pyreprfmt` entry, the raw prefix reached the wire.
    clear_cache()
    _warm_and_wait(repo_dir, "py_r")
    sym_files = [p for p in CACHE_DIR.glob("sym-*") if not p.name.endswith(".lock")]
    has_pyreprfmt = any("pyreprfmt" in p.read_text() for p in sym_files)
    results["6.3 raw (unnormalized) prefix sent on the wire"] = has_pyreprfmt

    # §6.4 — case-insensitive unconditionally: `MountTable` and `mounttable`
    # must return byte-identical results, not "whichever case fzf feels
    # like today".
    clear_cache()
    _warm_and_wait(repo_dir, "moun")
    lower_paths, _, _ = invoke(repo_dir, "mounttable")
    upper_paths, _, _ = invoke(repo_dir, "MountTable")
    results["6.4 case-insensitive matching"] = lower_paths == upper_paths and len(lower_paths) > 0

    # §6.5 — no result-count gate: the historical bug was an exact `local <
    # 5` gate that happened to be false (5 !< 5) for `mounttable`'s 5 fuzzy
    # hits, silently disabling the symbol channel for the flagship query.
    # Reproduce precisely that shape and confirm the right file still wins.
    clear_cache()
    _warm_and_wait(repo_dir, "moun")
    paths, _, _ = invoke(repo_dir, "mounttable")
    results["6.5 no local-hit-count gate (mounttable's exact historical case)"] = (
        bool(paths) and paths[0] == "crates/monty-fs/src/mount_table.rs"
    )

    # §6.6 — definition beats re-export. Deliberately query `mountt`, not
    # `mounttable`: the latter also exact-matches mount_table.rs's basename
    # and would pass via §6.1 alone, masking whether the symbol-tier
    # eponymous tiebreak actually works. `mountt` has no basename-exact hit
    # in this repo, so this genuinely exercises the tiebreak.
    clear_cache()
    _warm_and_wait(repo_dir, "moun")
    paths, _, _ = invoke(repo_dir, "mountt")
    def_idx = paths.index("crates/monty-fs/src/mount_table.rs") if "crates/monty-fs/src/mount_table.rs" in paths else None
    reexport_idx = paths.index("crates/monty-fs/src/lib.rs") if "crates/monty-fs/src/lib.rs" in paths else None
    results["6.6 definition ranks above re-export"] = (
        def_idx is not None and (reexport_idx is None or def_idx < reexport_idx)
    )

    all_pass = True
    for name, ok in results.items():
        status = "PASS" if ok else "FAIL"
        all_pass &= ok
        print(f"  [{status}] {name}")
    return all_pass


# --- Robustness: spec §2's "never panic, exit 0 always" ------------------


def run_robustness(repo_dir):
    """Exercises spec §2's hard requirement directly: exit 0, print nothing
    to stdout, on every malformed/edge-case input. The multi-byte case here
    is a real bug found while building this harness — `prefix_of` used to
    byte-slice the query at a fixed offset, which panics (exit 101) the
    instant a query contains a non-ASCII character whose bytes straddle that
    offset (e.g. a CJK filename search)."""
    print("\n=== Robustness (spec §2) ===")
    cases = [
        ("multi-byte query (regression: used to panic, see symbols::prefix_of)", "日本語ですよ", None),
        ("empty stdin", None, ""),
        ("malformed JSON", None, "not json{{{"),
        ("missing query field", None, "{}"),
        ("empty query", "", None),
    ]
    all_pass = True
    for name, query, raw_stdin in cases:
        env = {**os.environ, "CLAUDE_PROJECT_DIR": str(repo_dir)}
        stdin_data = raw_stdin if raw_stdin is not None else json.dumps({"query": query})
        result = subprocess.run([str(BINARY)], input=stdin_data, capture_output=True, text=True, env=env, timeout=5)
        ok = result.returncode == 0 and result.stdout == ""
        status = "PASS" if ok else "FAIL"
        all_pass &= ok
        print(f"  [{status}] {name}  (exit={result.returncode}, stdout={result.stdout!r})")

    # Not inside a git work tree: exit 0, no output (spec §2).
    non_git_dir = Path(tempfile.mkdtemp(prefix="file-suggestion-non-git-"))
    try:
        paths, _, rc = invoke(non_git_dir, "heap")
        ok = rc == 0 and paths == []
        status = "PASS" if ok else "FAIL"
        all_pass &= ok
        print(f"  [{status}] not inside a git work tree  (exit={rc}, paths={paths})")
    finally:
        shutil.rmtree(non_git_dir, ignore_errors=True)

    return all_pass


# --- Latency (spec §10) ---------------------------------------------------


def percentile(values, pct):
    values = sorted(values)
    idx = min(len(values) - 1, int(len(values) * pct))
    return values[idx]


def _spin_up_dummy_sg_server():
    """A local stand-in for Sourcegraph that always answers instantly with an
    empty result set. Used only to keep the *symbol-cache-cold* latency
    measurement from hammering the real sourcegraph.com 200x/run — the
    self-spawned `--warm` fetch is detached and never on the timed path
    either way, so which server it talks to cannot affect the number below."""

    class EmptyResultHandler(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.dumps({"data": {"search": {"results": {"results": []}}}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, fmt, *args):
            pass

    server = http.server.HTTPServer(("127.0.0.1", 0), EmptyResultHandler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, f"http://127.0.0.1:{server.server_port}"


def clear_symbol_cache_only():
    """Remove just the sym-*/lock files, leaving the file-list/recency/repo
    caches warm. Used for the §10 'cold cache' measurement — see the long
    comment in run_latency for why this, not a full `clear_cache()`, is the
    scenario that measurement is about."""
    if not CACHE_DIR.exists():
        return
    for p in CACHE_DIR.glob("sym-*"):
        p.unlink(missing_ok=True)


def run_latency(repo_dir, n=200):
    print("\n=== Latency ===")
    queries = ["heap", "dropg", "resolve_path", "mounttable", "py_r", "collect_cy", "xz"]

    # Warm: every cache (file list, recency, symbols) already populated.
    clear_cache()
    invoke(repo_dir, "heap")  # populate files/recent caches once
    for prefix in {"drop", "reso", "moun", "py_r", "coll"}:
        subprocess.run(
            [str(BINARY), "--warm", prefix],
            env={**os.environ, "CLAUDE_PROJECT_DIR": str(repo_dir)},
            timeout=30,
        )
    warm_times = []
    for i in range(n):
        _, ms, _ = invoke(repo_dir, queries[i % len(queries)])
        warm_times.append(ms)
    warm_p50, warm_p95 = percentile(warm_times, 0.50), percentile(warm_times, 0.95)
    print(f"  warm: p50={warm_p50:.2f}ms  p95={warm_p95:.2f}ms  (n={n}, target p95<15ms)")

    # Cold, spec §10 sense: the file-list and recency caches — which spec §4a/§4b
    # invalidate only on `.git/index` or HEAD changing, i.e. rarely — are
    # already warm from ordinary use of the repo. What's cold is the *symbol*
    # cache for whichever prefix was just typed, which is the cold case a user
    # actually hits on every new word, over and over, all session. That path
    # must stay fast because §5 forbids ever blocking on the fetch: emit
    # local-only results immediately and self-spawn `--warm` detached.
    #
    # A literal "delete every cache and re-measure" reading of "cold" is not
    # achievable under 25ms and contradicts spec §1's own numbers: `git
    # ls-files` alone on this repo measures ~30ms, and §4a exists specifically
    # to make that a one-time cost, not a per-keystroke one. We measure that
    # one-time cost separately below, labeled honestly as exceeding the
    # per-keystroke budget by design.
    dummy_server, dummy_endpoint = _spin_up_dummy_sg_server()
    try:
        clear_cache()
        invoke(repo_dir, "heap")  # establish the file-list/recency caches once
        cold_times = []
        for i in range(n):
            clear_symbol_cache_only()
            _, ms, _ = invoke(
                repo_dir,
                queries[i % len(queries)],
                env_extra={"CLAUDE_SG_ENDPOINT": dummy_endpoint},
            )
            cold_times.append(ms)
    finally:
        dummy_server.shutdown()
    cold_p50, cold_p95 = percentile(cold_times, 0.50), percentile(cold_times, 0.95)
    print(f"  cold (symbol cache cold, file/recency caches warm): p50={cold_p50:.2f}ms  p95={cold_p95:.2f}ms  (n={n}, target p95<25ms)")

    # First-ever invocation in a repo: every cache empty, including file list
    # and recency, so this pays `git ls-files` + `git log` once. Reported
    # separately and honestly — it is not, and cannot be, under 25ms.
    clear_cache()
    _, first_ms, _ = invoke(repo_dir, "heap")
    print(f"  first-ever invocation (empty cache dir, pays git ls-files + git log once): {first_ms:.2f}ms")

    return warm_p95, cold_p95, first_ms


# --- Security (spec §10, §5) ----------------------------------------------

captured_headers = []


class CaptureHandler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        # HTTP header names are case-insensitive on the wire (ureq sends
        # `authorization`, lowercase); normalize to lowercase here so the
        # assertions below can't fail on casing alone.
        captured_headers.append({k.lower(): v for k, v in self.headers.items()})
        body = json.dumps({"data": {"search": {"results": {"results": []}}}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        pass  # keep test output quiet


def _wait_for(predicate, timeout=5.0, interval=0.05):
    """Poll `predicate` until true or `timeout` elapses. `--warm` is spawned
    detached, so the test has no other signal for "the request landed"."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(interval)
    return predicate()


def run_security(repo_dir):
    print("\n=== Security (spec §5, §10) ===")
    server = http.server.HTTPServer(("127.0.0.1", 0), CaptureHandler)
    port = server.server_port
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    local_endpoint = f"http://127.0.0.1:{port}"

    all_pass = True
    try:
        # Case 1 (the shipped bug, spec §5/§10): SRC_ENDPOINT names a
        # *different* host than the one we're calling -> no Authorization
        # header may be attached, even though a token is present.
        captured_headers.clear()
        clear_cache()
        subprocess.run(
            [str(BINARY), "--warm", "auth"],
            env={
                **os.environ,
                "CLAUDE_PROJECT_DIR": str(repo_dir),
                "CLAUDE_SG_ENDPOINT": local_endpoint,
                "SRC_ENDPOINT": "https://demo.sourcegraph.com",
                "SRC_ACCESS_TOKEN": "leaked-demo-token-should-never-appear",
            },
            timeout=10,
        )
        _wait_for(lambda: len(captured_headers) >= 1)
        mismatched_ok = len(captured_headers) == 1 and "authorization" not in captured_headers[0]
        status = "PASS" if mismatched_ok else "FAIL"
        all_pass &= mismatched_ok
        print(f"  [{status}] SRC_ENDPOINT != endpoint called -> no Authorization header sent")
        if captured_headers:
            print(f"         headers seen: {list(captured_headers[0].keys())}")

        # Case 2: SRC_ENDPOINT matches the endpoint being called -> the
        # header IS attached (proves the negative test isn't just "always
        # off").
        captured_headers.clear()
        clear_cache()
        subprocess.run(
            [str(BINARY), "--warm", "auth"],
            env={
                **os.environ,
                "CLAUDE_PROJECT_DIR": str(repo_dir),
                "CLAUDE_SG_ENDPOINT": local_endpoint,
                "SRC_ENDPOINT": local_endpoint,
                "SRC_ACCESS_TOKEN": "matched-token",
            },
            timeout=10,
        )
        _wait_for(lambda: len(captured_headers) >= 1)
        matched_ok = (
            len(captured_headers) == 1
            and captured_headers[0].get("authorization") == "token matched-token"
        )
        status = "PASS" if matched_ok else "FAIL"
        all_pass &= matched_ok
        print(f"  [{status}] SRC_ENDPOINT == endpoint called -> Authorization header sent")
    finally:
        server.shutdown()
    return all_pass


def main():
    if len(sys.argv) != 2:
        print("usage: test_harness.py <path-to-monty-checkout>", file=sys.stderr)
        sys.exit(2)
    repo_dir = Path(sys.argv[1]).resolve()
    if not BINARY.exists():
        print(f"binary not found at {BINARY}; build+install it first", file=sys.stderr)
        sys.exit(2)

    ranking_ok = run_ranking(repo_dir)
    regressions_ok = run_regressions(repo_dir)
    robustness_ok = run_robustness(repo_dir)
    warm_p95, cold_p95, first_ms = run_latency(repo_dir)
    security_ok = run_security(repo_dir)

    print("\n=== Summary ===")
    print(f"  ranking:     {'PASS' if ranking_ok else 'FAIL'}")
    print(f"  regressions: {'PASS' if regressions_ok else 'FAIL'}")
    print(f"  robustness:  {'PASS' if robustness_ok else 'FAIL'}")
    print(
        f"  latency:  warm p95={warm_p95:.2f}ms (<15ms target), "
        f"cold p95={cold_p95:.2f}ms (<25ms target), "
        f"first-ever invocation={first_ms:.2f}ms (informational, see run_latency comment)"
    )
    print(f"  security: {'PASS' if security_ok else 'FAIL'}")


if __name__ == "__main__":
    main()
