/**
 * Minimal Sourcegraph Search Job runner: create a job, poll it to completion,
 * stream the results to disk as JSONL. Same result as search_job.ts, with none
 * of the terminal UI.
 *
 *   POST /api/searchjobs.v1.Service/CreateSearchJob   (scope: externalapi:write)
 *   POST /api/searchjobs.v1.Service/GetSearchJob      (scope: externalapi:read)
 *   GET  <SearchJob.resultsUrl>                        -> JSONL
 *
 * Usage:
 *   export SRC_ENDPOINT="https://demo.sourcegraph.com"
 *   export SRC_ACCESS_TOKEN="sgp_..."                    # read + write
 *
 *   node search_job_minimal.ts
 *   node search_job_minimal.ts --query 'context:global TODO count:all' --out todos.jsonl
 *   node search_job_minimal.ts --out - | jq -r .path | sort -u
 *
 * Status goes to stderr, results go to stdout, so `--out -` pipes cleanly.
 *
 * ES module. Runs on Node 22.18+ or 24.3+, where type stripping is on by
 * default. No dependencies.
 */

import { createWriteStream } from "node:fs";
import { Readable } from "node:stream";
import { finished } from "node:stream/promises";
import { setTimeout as sleep } from "node:timers/promises";

/** Mirrors the fields of searchjobs.v1.SearchJob that we actually read. */
interface SearchJob {
  name: string; // users/{user}/searchJobs/{id}
  state: string; // STATE_QUEUED | STATE_PROCESSING | STATE_COMPLETED | ...
  resultsUrl?: string;
  logsUrl?: string;
}

const flag = (name: string, fallback: string): string => {
  const i = process.argv.indexOf(name);
  return i >= 0 ? (process.argv[i + 1] ?? fallback) : fallback;
};

const query = flag("--query", "context:global patterntype:keyword TODO count:all");
const out = flag("--out", "results.jsonl");
const pollMs = Number(flag("--poll", "5")) * 1000;
const timeoutMs = Number(flag("--timeout", "1800")) * 1000;

const endpoint = (process.env.SRC_ENDPOINT ?? "https://demo.sourcegraph.com").replace(/\/+$/, "");
const token = process.env.SRC_ACCESS_TOKEN;
if (!token) {
  // The most common first-run mistake, so fail with a message instead of a stack.
  console.error("SRC_ACCESS_TOKEN is not set. Create a token with externalapi:read and externalapi:write scopes.");
  process.exit(1);
}

const auth = { Authorization: `token ${token}` };

async function rpc<T>(method: string, body: unknown): Promise<T> {
  const res = await fetch(`${endpoint}/api/searchjobs.v1.Service/${method}`, {
    method: "POST",
    headers: { ...auth, "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`${method} failed: HTTP ${res.status}: ${(await res.text()).trim()}`);
  return (await res.json()) as T;
}

// 1. Create the job. parent "users/-" means the authenticated user.
let job = await rpc<SearchJob>("CreateSearchJob", { parent: "users/-", searchJob: { query } });
console.error(`created ${job.name}`);

// 2. Poll until the job reaches a terminal state.
const deadline = Date.now() + timeoutMs;
while (job.state !== "STATE_COMPLETED") {
  if (job.state === "STATE_FAILED" || job.state === "STATE_CANCELED") {
    throw new Error(`job ended in ${job.state} (logs: ${job.logsUrl ?? "n/a"})`);
  }
  if (Date.now() > deadline) throw new Error(`timed out waiting for job (last state ${job.state})`);
  await sleep(pollMs);
  job = await rpc<SearchJob>("GetSearchJob", { name: job.name });
  console.error(`  ${job.state}`);
}

// 3. Stream the JSONL results to disk. Never buffers the whole result set.
if (!job.resultsUrl) throw new Error("job completed but no resultsUrl was returned");
const res = await fetch(new URL(job.resultsUrl, `${endpoint}/`), { headers: auth });
if (!res.ok) throw new Error(`results fetch failed: HTTP ${res.status}`);
if (!res.body) throw new Error("results response had no body");

const toFile = out !== "-";
const sink = toFile ? createWriteStream(out) : process.stdout;
let bytes = 0;
for await (const chunk of Readable.fromWeb(res.body as any)) {
  sink.write(chunk);
  bytes += (chunk as Buffer).length;
}
if (toFile) {
  sink.end();
  await finished(sink);
}
console.error(`wrote ${bytes} bytes to ${toFile ? out : "stdout"}`);
