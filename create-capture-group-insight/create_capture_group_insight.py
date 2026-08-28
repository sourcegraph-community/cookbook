a!/usr/bin/env python3
"""Create a Sourcegraph capture-group code insight scoped to an explicit repo list.

This builds a `createLineChartSearchInsight` mutation whose
`repositoryScope.repositories` is a literal list of repo names.

The query, title and repo list are all arguments, so one invocation shape covers
any "break this pattern out into a series per captured value" use case. See the
EXAMPLES below, or --help.

Auth: SRC_ENDPOINT + SRC_ACCESS_TOKEN (same env vars as src-cli), or
--endpoint / --token.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import ssl
import urllib.error
import urllib.request

EXAMPLES = r"""
The captured values are the series, so what you wrap in the group decides what
the chart breaks down by. For a requirements.txt pin like `django==5.0.2`:

  # one series per package
  --title "Pinned packages" \
    --query 'patterntype:regexp file:requirements\.txt$ ^([\w.-]+)==[\w.]+'
  # one series per version -- same match, different group
  --title "Django versions" \
    --query 'patterntype:regexp file:requirements\.txt$ ^django==([\w.]+)'
  # one series per package+version pair, spanning both in a single group
  --title "Pinned packages and versions" \
    --query 'patterntype:regexp file:requirements\.txt$ ^([\w.-]+==[\w.]+)'

The series label is the raw captured text. A group that spans a line break
captures the whitespace too, so matches differing only in indentation land in
separate series -- worth avoiding when a single group has to cross lines.

Other shapes:

  # Go toolchain version per repo
  --title "Go versions" --query 'patterntype:regexp file:^go\.mod$ ^go (\d+\.\d+)'
  # Base images in Dockerfiles; (?:...) keeps the optional flag out of the label
  --title "Base images" \
    --query 'patterntype:regexp file:Dockerfile$ ^FROM (?:--platform=\S+ )?(\S+)'
  # CI runners in use
  --title "Workflow runners" \
    --query 'patterntype:regexp file:^\.github/workflows/ runs-on: (\S+)'

Workflow: check the repo list and see what series you would get before writing
anything, then create it.

  ./create_capture_group_insight.py --repos-file repos.txt --title T --query Q \
      --validate-repos --preview --dry-run
  ./create_capture_group_insight.py --repos-file repos.txt --title T --query Q \
      --dashboard "Platform migration"
"""

CREATE_INSIGHT = """
mutation CreateCaptureGroupInsight($input: LineChartSearchInsightInput!) {
  createLineChartSearchInsight(input: $input) {
    view {
      id
      presentation { ... on LineChartInsightViewPresentation { title } }
    }
  }
}
"""

PREVIEW = """
query PreviewCaptureGroupInsight($input: SearchInsightPreviewInput!) {
  searchInsightPreview(input: $input) {
    label
    points { dateTime value }
  }
}
"""

DASHBOARDS = """
query Dashboards($after: String) {
  insightsDashboards(first: 100, after: $after) {
    nodes { id title }
    pageInfo { endCursor hasNextPage }
  }
}
"""


class GraphQLError(RuntimeError):
    pass


def count_capture_groups(pattern: str) -> int:
    """Count capturing groups in a regexp, ignoring escapes and (?:...) groups."""
    count = 0
    i = 0
    in_class = False
    while i < len(pattern):
        ch = pattern[i]
        if ch == "\\":
            i += 2
            continue
        if in_class:
            in_class = ch != "]"
        elif ch == "[":
            in_class = True
        elif ch == "(":
            nxt = pattern[i + 1 : i + 2]
            # (?P<name>...) captures; every other (?...) form does not.
            if nxt != "?" or pattern[i + 2 : i + 3] in ("P", "<"):
                count += 1
        i += 1
    return count


def load_query(args: argparse.Namespace) -> str:
    """Resolve the query from --query or --query-file (exactly one is required)."""
    if bool(args.query) == bool(args.query_file):
        raise SystemExit("pass exactly one of --query or --query-file")
    if args.query:
        return args.query.strip()
    if args.query_file == "-":
        text = sys.stdin.read()
    else:
        with open(args.query_file, encoding="utf-8") as fh:
            text = fh.read()
    # Deliberately no line joining: a regexp can contain a literal space, so
    # stitching wrapped lines back together would silently alter the pattern.
    lines = [line for line in text.splitlines() if line.strip() and not line.startswith("#")]
    if len(lines) != 1:
        raise SystemExit(
            f"expected exactly one query line (# comments and blanks ignored), got {len(lines)}"
        )
    return lines[0].strip()


def check_capture_groups(query: str) -> None:
    groups = count_capture_groups(query)
    if groups == 1:
        return
    if groups == 0:
        raise SystemExit(
            "the query has no capturing group; a capture-group insight needs exactly one"
        )
    print(
        f"warning: the query has {groups} capturing groups. Capture-group insights count "
        "every captured value, so each group's values become their own interleaved series "
        "(e.g. names mixed in with version strings). Keep only the group you want "
        "capturing and make the rest non-capturing, e.g. (?:...).",
        file=sys.stderr,
    )


# Server-side limit in live_preview_resolvers.go; previews only, not the insight.
MAX_PREVIEW_REPOS = 20

CA_FALLBACKS = ("/etc/ssl/cert.pem", "/usr/local/etc/openssl/cert.pem")


def ssl_context(ca_bundle: str | None) -> ssl.SSLContext:
    """Default trust store, with a fallback for pythons that ship without one.

    python.org builds on macOS have an empty default store until
    `Install Certificates.command` is run; fall back to the system bundle so
    the script works out of the box.
    """
    if ca_bundle:
        return ssl.create_default_context(cafile=ca_bundle)
    ctx = ssl.create_default_context()
    if ctx.get_ca_certs():
        return ctx
    try:
        import certifi

        ctx.load_verify_locations(cafile=certifi.where())
        return ctx
    except ImportError:
        pass
    for path in CA_FALLBACKS:
        if os.path.exists(path):
            ctx.load_verify_locations(cafile=path)
            break
    return ctx


class Timeout(GraphQLError):
    """The request outlived the client timeout or an upstream gateway's."""


GATEWAY_TIMEOUTS = {502, 503, 504, 520, 522, 524}

class Client:
    def __init__(
        self,
        endpoint: str,
        token: str,
        ca_bundle: str | None = None,
        timeout: float | None = None,
    ) -> None:
        self.url = endpoint.rstrip("/") + "/.api/graphql"
        self.token = token
        self.context = ssl_context(ca_bundle)
        self.timeout = timeout

    def request(self, query: str, variables: dict) -> dict:
        body = json.dumps({"query": query, "variables": variables}).encode()
        req = urllib.request.Request(
            self.url,
            data=body,
            headers={
                "Authorization": f"token {self.token}",
                "Content-Type": "application/json",
                # The default urllib User-Agent gets a 403 from some instances.
                "User-Agent": "create-capture-group-insight/1.0",
            },
        )
        try:
            with urllib.request.urlopen(req, context=self.context, timeout=self.timeout) as resp:
                payload = json.load(resp)
        except urllib.error.HTTPError as err:
            detail = err.read().decode(errors="replace")[:2000]
            if err.code in GATEWAY_TIMEOUTS:
                raise Timeout(
                    f"HTTP {err.code}: the instance did not respond in time (the request was "
                    "still running when the gateway gave up)"
                ) from None
            raise GraphQLError(f"HTTP {err.code} from {self.url}: {detail}") from None
        except TimeoutError:
            raise Timeout(f"no response within {self.timeout}s") from None
        except urllib.error.URLError as err:
            if isinstance(err.reason, TimeoutError):
                raise Timeout(f"no response within {self.timeout}s") from None
            hint = ""
            if isinstance(err.reason, ssl.SSLCertVerificationError):
                hint = " (no usable CA bundle: pass --ca-bundle or set SSL_CERT_FILE)"
            raise GraphQLError(f"cannot reach {self.url}: {err.reason}{hint}") from None
        if payload.get("errors"):
            msgs = "; ".join(e.get("message", str(e)) for e in payload["errors"])
            raise GraphQLError(msgs)
        return payload["data"]


def normalize_repo(line: str) -> str:
    """Accept a plain repo name, or one pasted out of a search query.

    `repo:^github\\.com/acme/foo$` and `"github.com/acme/foo",` both normalize
    to `github.com/acme/foo`.
    """
    name = line.strip().strip(",").strip().strip("'\"")
    for prefix in ("repo:", "r:"):
        if name.startswith(prefix):
            name = name[len(prefix) :]
    name = name.removeprefix("https://").removesuffix("/")
    if name.startswith("^"):
        name = name[1:]
    if name.endswith("$"):
        name = name[:-1]
    return name.replace(r"\.", ".").replace(r"\-", "-")


def load_repos(args: argparse.Namespace) -> list[str]:
    raw: list[str] = list(args.repo)
    for path in args.repos_file:
        text = sys.stdin.read() if path == "-" else open(path, encoding="utf-8").read()
        raw.extend(text.splitlines())

    repos: list[str] = []
    for line in raw:
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        name = normalize_repo(line)
        if name and name not in repos:
            repos.append(name)
    return repos


def validate_repos(client: Client, repos: list[str], batch: int = 50) -> list[str]:
    """Return the repo names the instance does not know about."""
    missing: list[str] = []
    for start in range(0, len(repos), batch):
        chunk = repos[start : start + batch]
        decls = ", ".join(f"$n{i}: String!" for i in range(len(chunk)))
        fields = "\n".join(
            f"  r{i}: repository(name: $n{i}) {{ name }}" for i in range(len(chunk))
        )
        query = f"query CheckRepos({decls}) {{\n{fields}\n}}"
        variables = {f"n{i}": name for i, name in enumerate(chunk)}
        data = client.request(query, variables)
        for i, name in enumerate(chunk):
            if data.get(f"r{i}") is None:
                missing.append(name)
    return missing


def resolve_dashboard(client: Client, wanted: str) -> str:
    """Resolve a dashboard by title; pass an ID through unchanged."""
    seen: list[str] = []
    after = None
    while True:
        conn = client.request(DASHBOARDS, {"after": after})["insightsDashboards"]
        for node in conn["nodes"]:
            if wanted in (node["id"], node["title"]):
                return node["id"]
            seen.append(node["title"])
        if not conn["pageInfo"]["hasNextPage"]:
            break
        after = conn["pageInfo"]["endCursor"]
    raise SystemExit(
        f"dashboard {wanted!r} not found. Visible dashboards: {', '.join(sorted(seen)) or '(none)'}"
    )


def build_input(args: argparse.Namespace, repos: list[str], dashboard_id: str | None) -> dict:
    payload = {
        "options": {"title": args.title},
        "repositoryScope": {"repositories": repos},
        "timeScope": {
            "stepInterval": {"unit": args.interval_unit, "value": args.interval_value}
        },
        "dataSeries": [
            {
                "query": args.query,
                "options": {"label": args.series_label},
                "generatedFromCaptureGroups": True,
            }
        ],
        "viewControls": {
            "filters": {},
            "seriesDisplayOptions": {
                "limit": args.series_limit,
                "numSamples": args.num_samples,
                "sortOptions": {"mode": args.sort_mode, "direction": args.sort_direction},
            },
        },
    }
    if dashboard_id:
        payload["dashboards"] = [dashboard_id]
    return payload


def preview_scope(args: argparse.Namespace, repos: list[str]) -> list[str]:
    """Pick the repositories to preview against.

    The preview is a check on the query, not a census, so it runs over a small
    slice of the insight's scope: --preview-repo names it explicitly, otherwise
    the first --preview-repos of the list. The resolver hard-errors above
    MAX_PREVIEW_REPOS, so that is also the ceiling here.
    """
    if args.preview_repo:
        scope: list[str] = []
        for name in (normalize_repo(r) for r in args.preview_repo):
            if name not in scope:
                scope.append(name)
        outside = [name for name in scope if name not in set(repos)]
        if outside:
            print(
                "preview: these --preview-repo names are not in the insight's repo list, "
                f"so the preview is not representative of it: {', '.join(outside)}",
                file=sys.stderr,
            )
    else:
        scope = repos

    limit = args.preview_repos
    if limit > MAX_PREVIEW_REPOS:
        print(
            f"preview: the API caps previews at {MAX_PREVIEW_REPOS} repositories, "
            f"ignoring --preview-repos {limit}",
            file=sys.stderr,
        )
        limit = MAX_PREVIEW_REPOS
    if len(scope) > limit:
        print(
            f"preview: sampling the first {limit} of {len(scope)} repositories "
            "(--preview-repos to change, --preview-repo to choose them)",
            file=sys.stderr,
        )
    return scope[:limit]


def run_preview(client: Client, args: argparse.Namespace, repos: list[str]) -> None:
    def latest_value(entry: dict) -> float:
        if not entry["points"]:
            return 0.0
        return max(entry["points"], key=lambda p: p["dateTime"])["value"]

    scope = preview_scope(args, repos)

    try:
        data = client.request(PREVIEW, preview_input(args, scope))
    except Timeout as err:
        # The preview runs len(scope) * 7 searches inline, each pinned to a
        # historical commit and therefore unindexed, so cost scales with repo
        # size as well as count.
        noun = "repo" if len(scope) == 1 else "repos"
        if len(scope) > 1:
            advice = f"Retry with --preview-repos {max(1, len(scope) // 4)}, or "
        else:
            advice = "Retry with "
        print(
            f"preview: {err}\n"
            f"  It ran {len(scope)} {noun} x 7 historical searches in one request, each pinned "
            "to an old commit and so unindexed.\n"
            f"  {advice}--preview-repo NAME to name a small repo you know contains the "
            "pattern.\n"
            "  This says nothing about the query or the insight: the insight backfills in the "
            "background and has no such limit.",
            file=sys.stderr,
        )
        raise SystemExit(1) from None
    except GraphQLError as err:
        # The resolver reports "no points in any series" as an error rather than
        # an empty list (live_preview_resolvers.go: noDataErrorCode).
        if "not found" not in str(err):
            raise
        where = "the repository" if len(scope) == 1 else f"any of the {len(scope)} repositories"
        print(
            f"preview: no matches in {where} over the requested time range",
            file=sys.stderr,
        )
        return

    series = data["searchInsightPreview"]
    print(f"preview: {len(series)} series generated from captured values")
    for entry in sorted(series, key=latest_value, reverse=True):
        # Collapse whitespace so multi-line capture labels stay readable here;
        # the series label itself keeps the raw matched text.
        label = " ".join(entry["label"].split())
        print(f"  {label}: latest={latest_value(entry):g}")


def preview_input(args: argparse.Namespace, repos: list[str]) -> dict:
    return {
        "input": {
            "repositoryScope": {"repositories": repos},
            "timeScope": {
                "stepInterval": {"unit": args.interval_unit, "value": args.interval_value}
            },
            "series": [
                {
                    "query": args.query,
                    "label": args.series_label,
                    "generatedFromCaptureGroups": True,
                }
            ],
        }
    }


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__,
        epilog="EXAMPLES" + EXAMPLES,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--repos-file",
        action="append",
        default=[],
        metavar="PATH",
        help="file with one repo name per line ('-' for stdin; # comments and blanks ignored). Repeatable.",
    )
    p.add_argument(
        "--repo", action="append", default=[], metavar="NAME", help="repo name. Repeatable."
    )
    p.add_argument("--title", required=True, help="insight title, shown on the chart")
    p.add_argument(
        "--query",
        help="search query with exactly one capturing group. Single-quote it so the "
        "shell leaves the regexp alone.",
    )
    p.add_argument(
        "--query-file",
        metavar="PATH",
        help="read the query from a file instead ('-' for stdin); avoids shell quoting entirely",
    )
    p.add_argument(
        "--series-label",
        default="captured value",
        help="placeholder label; capture-group insights overwrite it with the captured "
        "values (default: %(default)s)",
    )
    p.add_argument(
        "--interval-unit",
        default="MONTH",
        choices=["HOUR", "DAY", "WEEK", "MONTH", "YEAR"],
    )
    p.add_argument("--interval-value", type=int, default=1)
    p.add_argument(
        "--series-limit",
        type=int,
        default=20,
        help="max dynamically generated series to display (default: 20)",
    )
    p.add_argument("--num-samples", type=int, default=12, help="points per series (default: 12)")
    p.add_argument(
        "--sort-mode",
        default="RESULT_COUNT",
        choices=["RESULT_COUNT", "LEXICOGRAPHICAL", "DATE_ADDED"],
    )
    p.add_argument("--sort-direction", default="DESC", choices=["ASC", "DESC"])
    p.add_argument("--dashboard", metavar="TITLE_OR_ID", help="attach the insight to this dashboard")
    p.add_argument(
        "--validate-repos",
        action="store_true",
        help="check every repo name resolves on the instance before creating",
    )
    p.add_argument(
        "--allow-missing-repos",
        action="store_true",
        help="with --validate-repos, warn instead of aborting (unknown names are dropped)",
    )
    p.add_argument(
        "--preview",
        action="store_true",
        help="run searchInsightPreview and print the series that would be generated",
    )
    p.add_argument(
        "--preview-repos",
        type=int,
        default=10,
        metavar="N",
        help=f"how many of the repos to preview against, max {MAX_PREVIEW_REPOS} "
        "(default: %(default)s). The insight itself always gets the full list.",
    )
    p.add_argument(
        "--preview-repo",
        action="append",
        default=[],
        metavar="NAME",
        help="preview against these repos specifically instead of the first N. Repeatable.",
    )
    p.add_argument("--dry-run", action="store_true", help="print the mutation input and exit")
    p.add_argument(
        "--ca-bundle",
        default=os.environ.get("SSL_CERT_FILE"),
        metavar="PATH",
        help="CA bundle for TLS verification (default: $SSL_CERT_FILE, else the system store)",
    )
    p.add_argument(
        "--timeout",
        type=float,
        default=180.0,
        metavar="SECONDS",
        help="per-request timeout (default: %(default)s). A gateway in front of the "
        "instance may give up sooner.",
    )
    p.add_argument("--endpoint", default=os.environ.get("SRC_ENDPOINT"))
    p.add_argument("--token", default=os.environ.get("SRC_ACCESS_TOKEN"))
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    args.query = load_query(args)
    check_capture_groups(args.query)

    repos = load_repos(args)
    if not repos:
        raise SystemExit("no repositories given; use --repos-file and/or --repo")

    needs_api = not args.dry_run or args.validate_repos or args.preview or args.dashboard
    client = None
    if needs_api:
        if not args.endpoint or not args.token:
            raise SystemExit("set SRC_ENDPOINT and SRC_ACCESS_TOKEN (or pass --endpoint/--token)")
        client = Client(args.endpoint, args.token, args.ca_bundle, args.timeout)

    if args.validate_repos:
        missing = validate_repos(client, repos)
        if missing:
            listing = "\n".join(f"  {name}" for name in missing)
            if not args.allow_missing_repos:
                raise SystemExit(f"{len(missing)} repo(s) not found on the instance:\n{listing}")
            print(f"warning: dropping {len(missing)} unknown repo(s):\n{listing}", file=sys.stderr)
            repos = [r for r in repos if r not in set(missing)]
            if not repos:
                raise SystemExit("no known repositories left after validation")
        else:
            print(f"validated {len(repos)} repositories")

    if args.preview:
        run_preview(client, args, repos)

    dashboard_id = resolve_dashboard(client, args.dashboard) if args.dashboard else None
    insight_input = build_input(args, repos, dashboard_id)

    if args.dry_run:
        print(json.dumps(insight_input, indent=2))
        return 0

    view = client.request(CREATE_INSIGHT, {"input": insight_input})[
        "createLineChartSearchInsight"
    ]["view"]
    print(f"created insight {view['id']} ({len(repos)} repos)")
    print(f"  {args.endpoint.rstrip('/')}/insights/{view['id']}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except GraphQLError as err:
        print(f"error: {err}", file=sys.stderr)
        sys.exit(1)
