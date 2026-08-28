#!/usr/bin/env python3
"""Create a Sourcegraph search context from a file of repository names."""

from __future__ import annotations

import argparse
import json
import os
import ssl
import sys
import urllib.error
import urllib.request


CREATE_SEARCH_CONTEXT = """
mutation CreateSearchContext(
  $searchContext: SearchContextInput!
  $repositories: [SearchContextRepositoryRevisionsInput!]!
) {
  createSearchContext(searchContext: $searchContext, repositories: $repositories) {
    id
    spec
  }
}
"""

CURRENT_USER = """
query CurrentUser {
  currentUser { id username }
}
"""


class GraphQLError(RuntimeError):
    pass


CA_FALLBACKS = ("/etc/ssl/cert.pem", "/usr/local/etc/openssl/cert.pem")


def ssl_context(ca_bundle: str | None) -> ssl.SSLContext:
    """Build a TLS context using an explicit, Python, or system CA bundle."""
    if ca_bundle:
        return ssl.create_default_context(cafile=ca_bundle)
    context = ssl.create_default_context()
    if context.get_ca_certs():
        return context
    try:
        import certifi

        context.load_verify_locations(cafile=certifi.where())
        return context
    except ImportError:
        pass
    for path in CA_FALLBACKS:
        if os.path.exists(path):
            context.load_verify_locations(cafile=path)
            break
    return context


class Client:
    def __init__(
        self, endpoint: str, token: str, ca_bundle: str | None, timeout: float
    ) -> None:
        self.endpoint = endpoint.rstrip("/")
        self.url = self.endpoint + "/.api/graphql"
        self.token = token
        self.context = ssl_context(ca_bundle)
        self.timeout = timeout

    def request(self, query: str, variables: dict | None = None) -> dict:
        body = json.dumps({"query": query, "variables": variables or {}}).encode()
        request = urllib.request.Request(
            self.url,
            data=body,
            headers={
                "Authorization": f"token {self.token}",
                "Content-Type": "application/json",
                "User-Agent": "create-search-context/1.0",
            },
        )
        try:
            with urllib.request.urlopen(
                request, context=self.context, timeout=self.timeout
            ) as response:
                payload = json.load(response)
        except urllib.error.HTTPError as error:
            detail = error.read().decode(errors="replace")[:2000]
            raise GraphQLError(f"HTTP {error.code} from {self.url}: {detail}") from None
        except urllib.error.URLError as error:
            hint = ""
            if isinstance(error.reason, ssl.SSLCertVerificationError):
                hint = " (no usable CA bundle: pass --ca-bundle or set SSL_CERT_FILE)"
            raise GraphQLError(f"cannot reach {self.url}: {error.reason}{hint}") from None

        if payload.get("errors"):
            messages = "; ".join(
                error.get("message", str(error)) for error in payload["errors"]
            )
            raise GraphQLError(messages)
        return payload["data"]


def normalize_repo(line: str) -> str:
    """Normalize plain names and names copied from a Sourcegraph query."""
    name = line.strip().strip(",").strip().strip("'\"")
    for prefix in ("repo:", "r:"):
        if name.startswith(prefix):
            name = name[len(prefix) :]
    name = name.removeprefix("https://").removesuffix("/")
    name = name.removeprefix("^").removesuffix("$")
    return name.replace(r"\.", ".").replace(r"\-", "-")


def load_repos(path: str) -> list[str]:
    text = sys.stdin.read() if path == "-" else open(path, encoding="utf-8").read()
    repos: list[str] = []
    for line in text.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        name = normalize_repo(line)
        if name and name not in repos:
            repos.append(name)
    return repos


def resolve_repos(client: Client, repos: list[str], batch_size: int = 50) -> tuple[list[dict], list[str], list[str]]:
    """Resolve repository IDs and default branches, returning failures separately."""
    resolved: list[dict] = []
    missing: list[str] = []
    no_default_branch: list[str] = []

    for start in range(0, len(repos), batch_size):
        chunk = repos[start : start + batch_size]
        declarations = ", ".join(f"$name{i}: String!" for i in range(len(chunk)))
        fields = "\n".join(
            f"repo{i}: repository(name: $name{i}) {{ id name defaultBranch {{ abbrevName }} }}"
            for i in range(len(chunk))
        )
        query = f"query ResolveRepositories({declarations}) {{\n{fields}\n}}"
        variables = {f"name{i}": name for i, name in enumerate(chunk)}
        data = client.request(query, variables)

        for i, requested_name in enumerate(chunk):
            repository = data.get(f"repo{i}")
            if repository is None:
                missing.append(requested_name)
                continue
            default_branch = repository.get("defaultBranch")
            if not default_branch:
                no_default_branch.append(requested_name)
                continue
            resolved.append(
                {
                    "id": repository["id"],
                    "repository": repository["name"],
                    "revisions": [default_branch["abbrevName"]],
                }
            )

    return resolved, missing, no_default_branch


def print_skipped(label: str, repos: list[str]) -> None:
    if not repos:
        return
    print(f"warning: {len(repos)} {label}:", file=sys.stderr)
    for repo in repos:
        print(f"  {repo}", file=sys.stderr)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--name", required=True, help="search context name")
    parser.add_argument(
        "--repos-file",
        required=True,
        metavar="PATH",
        help="one repository per line; blank lines and # comments are ignored ('-' for stdin)",
    )
    parser.add_argument("--description", default="", help="search context description")
    parser.add_argument("--public", action="store_true", help="make the context public")
    parser.add_argument(
        "--global",
        dest="global_context",
        action="store_true",
        help="create an instance-level context instead of a user-owned context",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="resolve repositories and print the search context JSON without creating it",
    )
    parser.add_argument(
        "--ca-bundle",
        default=os.environ.get("SSL_CERT_FILE"),
        metavar="PATH",
        help="CA bundle for TLS verification (default: $SSL_CERT_FILE, else the system store)",
    )
    parser.add_argument("--endpoint", default=os.environ.get("SRC_ENDPOINT"))
    parser.add_argument("--token", default=os.environ.get("SRC_ACCESS_TOKEN"))
    parser.add_argument("--timeout", type=float, default=60.0, metavar="SECONDS")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    if not args.endpoint or not args.token:
        raise SystemExit("set SRC_ENDPOINT and SRC_ACCESS_TOKEN (or pass --endpoint/--token)")

    repos = load_repos(args.repos_file)
    if not repos:
        raise SystemExit("the repository file contains no repositories")

    client = Client(args.endpoint, args.token, args.ca_bundle, args.timeout)
    resolved, missing, no_default_branch = resolve_repos(client, repos)
    print_skipped("input repositories were not found in Sourcegraph and were skipped", missing)
    print_skipped("repositories had no default branch and were skipped", no_default_branch)
    if not resolved:
        raise SystemExit("no repositories with a default branch are available for the context")

    config = [
        {"repository": repo["repository"], "revisions": repo["revisions"]}
        for repo in resolved
    ]
    if args.dry_run:
        print(json.dumps(config, indent=2))
        return 0

    namespace = None
    if not args.global_context:
        current_user = client.request(CURRENT_USER).get("currentUser")
        if current_user is None:
            raise SystemExit("the access token does not identify an authenticated user")
        namespace = current_user["id"]

    variables = {
        "searchContext": {
            "name": args.name,
            "description": args.description,
            "public": args.public,
            "namespace": namespace,
            "query": "",
        },
        "repositories": [
            {"repositoryID": repo["id"], "revisions": repo["revisions"]}
            for repo in resolved
        ],
    }
    context = client.request(CREATE_SEARCH_CONTEXT, variables)["createSearchContext"]
    print(f"created context:{context['spec']} with {len(resolved)} repositories")
    print(f"{client.endpoint}/contexts/{context['spec']}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except GraphQLError as error:
        print(f"error: {error}", file=sys.stderr)
        sys.exit(1)
