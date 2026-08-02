# TODO debt radar

Build an owner-aware, scored inventory of `TODO`, `FIXME`, `HACK`, and `XXX` comments across Sourcegraph. The recipe runs exactly two exhaustive Search Jobs, preserves their raw JSONL, then performs token-free streaming analysis with no runtime dependencies.

## Prerequisites and quickstart

- Sourcegraph 7.0 or later.
- Node 22.18+ or 24.3+ (native TypeScript stripping). On Node 22.6–22.17 add `--experimental-strip-types`; on older Node use `npx tsx`.
- `SRC_ACCESS_TOKEN` with `externalapi:write` and `externalapi:read`; optional `SRC_ENDPOINT` (default `https://demo.sourcegraph.com`).

```sh
export SRC_ENDPOINT=https://sourcegraph.example.com
export SRC_ACCESS_TOKEN=sgp_...
node todo_debt_radar.ts > report.md
```

The default invocation collects and analyzes. Status goes to stderr, the Markdown report to stdout, enriched JSONL to `todo-debt.jsonl`, and raw inputs to `todo-debt-raw/`.

## Commands and flags

```sh
node todo_debt_radar.ts collect --raw-dir raw --poll 5 --timeout 1800
node todo_debt_radar.ts analyze --raw-dir raw --out debt.jsonl --top 25 --owner @backend > report.md
```

| Flag | Default | Meaning |
|---|---|---|
| `--raw-dir` | `todo-debt-raw` | Raw `debt.jsonl` and `codeowners.jsonl` boundary between phases. |
| `--out` | `todo-debt.jsonl` | Full enriched JSONL output. |
| `--top` | `20` | Number of highest scores retained for the report and unowned section. |
| `--owner` | none | Include only records with this exact owner; leading `@` is optional. |
| `--poll` | `5` seconds | Collection polling interval. |
| `--timeout` | `1800` seconds | Per-job timeout. |

`analyze` needs neither a token nor network. Records contain repository, path, commit, one-based line and column, comment text, marker, optional parenthetical author, ticket references, path class, all owners, a Sourcegraph link, and every score component.

## The two queries

```text
context:global patterntype:regexp (//|#|/\*|\*|<!--|--|;)[ \t]*(TODO|FIXME|HACK|XXX)\b count:all
context:global patterntype:regexp file:^(CODEOWNERS|\.github/CODEOWNERS|docs/CODEOWNERS)$ .+ count:all
```

The block displays regex backslashes escaped; each doubled backslash is submitted as one.

`count:all` retains all matches rather than normal ranked-search limits. This is essential both for a complete debt inventory and for reconstructing every nonempty CODEOWNERS line. The first RE2 expression requires a comment prefix and only horizontal whitespace before the marker. The second produces one match per nonempty line in the three conventional CODEOWNERS locations.

Collection creates and polls jobs at `/api/searchjobs.v1.Service/{CreateSearchJob,GetSearchJob}`, then streams each `resultsUrl` to disk. Raw JSONL is deliberately the stable on-disk handoff: archive it or repeatedly analyze it offline without rerunning searches.

## Ownership and scoring

Per repository, `.github/CODEOWNERS` wins over root `CODEOWNERS`, which wins over `docs/CODEOWNERS`. The matcher handles comments, ownerless overrides, rooted and unrooted patterns, `*`, `**`, and `?`. Among matching rules it chooses the longest literal path prefix, with the later line breaking ties, and retains multiple owners. Missing and explicitly ownerless matches are reported as unowned.

Total score is the sum of these visible components:

| Component | Points |
|---|---|
| Marker | TODO 2; FIXME 4; HACK/XXX 5 |
| Path class | production 4; test 2; docs 1; generated/vendored 0 |
| Dated comment age | whole years, capped at 5 |
| Low-number ticket era | 2 when a JIRA-style or `#` ticket number is 1–999 |

Dates and low ticket numbers are only staleness hints. The recipe performs no blame lookup and does not call ticket systems.

## Report and limitations

Analysis streams debt matches: only CODEOWNERS, rollup maps, top N records, and up to N highest-risk unowned examples remain in memory. The Markdown report includes totals, linked top context, repositories ranked by **markers per affected file (a density proxy, not repository-wide density)**, owner and marker-type rollups, and unowned examples. Markdown text is escaped.

Search ranges choose the extracted lines while the full chunk line provides context; duplicate ranges on one line are removed. The CODEOWNERS matcher intentionally covers common patterns rather than every host-specific parser edge case. Age is approximate and only available when a comment embeds a date. `TODO(name)` is an author only when the parenthetical resembles a person rather than a JIRA or `#123` reference.

## Tests

Tests use checked-in Search Jobs fixtures and need no token:

```sh
npx --yes tsx --test todo_debt_radar.test.ts
```
