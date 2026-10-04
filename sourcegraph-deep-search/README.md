# Sourcegraph Deep Search

A Claude Code mod that brings Sourcegraph Deep Search into the session. Type `/sourcegraph-deep-search <question>` and Deep Search's answer, with its sources, appears in a side pane.

Video: [Sourcegraph Deep Search in Claude Code](https://x.com/jdorfman/status/2106142195174785378)

## Prerequisites

- Claude Code with mods (function-hook plugins) available.
- Your Sourcegraph instance with Deep Search enabled. The mod has no default instance: you set its URL once, and the mod connects to `<your URL>/.api/mcp/deepsearch`.

## Quickstart

```sh
git clone https://github.com/sourcegraph-community/cookbook.git
claude --plugin-dir cookbook/sourcegraph-deep-search
```

When the plugin is enabled, Claude Code asks for **Sourcegraph URL**, e.g. `https://sourcegraph.example.com` (no trailing slash). To set or change it later, open `/config`, find the `sourcegraph-deep-search` row, then run `/reload-plugins`.

In the session, run `/mcp` and sign in to the `plugin:sourcegraph-deep-search:deepsearch` server (OAuth). Then:

```
/sourcegraph-deep-search where do Kubernetes repos still use the deprecated io/ioutil package?
```

To load it in every session, copy the directory to `~/.claude/mods/sourcegraph-deep-search`.

## What it does

- The mod adds the [Deep Search MCP endpoint](https://sourcegraph.com/docs/api/mcp) (`/.api/mcp/deepsearch`) to its manifest, and `/sourcegraph-deep-search` calls that endpoint's `deepsearch` tool with your question.
- The pane shows the question while Deep Search works, then renders the markdown answer and links to the conversation on Sourcegraph.
- Claude Code holds the connection and its OAuth credentials. The mod never sees a token.

## Files

| File | Purpose |
| --- | --- |
| `.claude-plugin/plugin.json` | Manifest: the Sourcegraph URL setting and the Deep Search MCP server built from it |
| `hooks/register.tsx` | The mod: command and pane |
| `hooks/spinner.tsx` | Animated spinner drawn while Deep Search works |
| `assets/` | Deep Search logo (PNG for the terminal, SVG elsewhere) |
| `types/index.d.ts` | Shape of the answer state the pane reads |
| `tests/sourcegraph-deep-search.test.ts` | Tests against a fake MCP server |

## Test

```sh
claude plugin validate .
claude plugin test .
```
