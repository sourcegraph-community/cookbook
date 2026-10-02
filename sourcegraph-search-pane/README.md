# Sourcegraph search pane

A Claude Code mod that puts Sourcegraph code search inside the session. Type `/sgs <query>` and the matches show up in a side pane as clickable `repo › path:line` links with a preview line. The same mod gives Claude a `search` tool, so it can look up real code across every indexed repo without cloning anything.

## Prerequisites

- Claude Code with mods (function-hook plugins) available.
- The [`src` CLI](https://github.com/sourcegraph/src-cli) on your `PATH`.
- `SRC_ENDPOINT` and `SRC_ACCESS_TOKEN` set for your Sourcegraph instance. The mod searches whatever instance `src` points at.

Check that `src` works before loading the mod:

```sh
src search -json 'repo:sourcegraph count:1 func main' | head -c 200
```

## Quickstart

```sh
git clone https://github.com/sourcegraph-community/cookbook.git
claude --plugin-dir cookbook/sourcegraph-search-pane
```

Then, in the session:

```
/sgs repo:sourcegraph lang:go func main
```

To load it in every session, copy the directory to `~/.claude/mods/sgs`.

## What it does

- `/sgs <query>` runs `src search -json` on your machine, opens the pane, and prints the result count and a link to the full results.
- The pane lists each line match as a link to that line on your instance, plus an "Open in Sourcegraph" link for the whole result set.
- Claude gets a `mcp__sgs__search` tool that takes the same query syntax and returns up to 40 matches as text.
- `count:50` is added unless the query sets its own `count:`.

Search runs through `src` rather than an HTTP call from the mod, so your token stays in your environment and the mod never reads it.

## Files

| File | Purpose |
| --- | --- |
| `hooks/register.tsx` | The mod: command, model tool, and pane |
| `types/index.d.ts` | Shape of the search state the pane reads |
| `tests/sgs.test.ts` | Tests against a fake `src` |

## Test

```sh
claude plugin validate .
claude plugin test .
```
