/**
 * Run an exhaustive Sourcegraph Search Job through the Sourcegraph API
 * (searchjobs.v1.Service), poll it to completion, and download the aggregated
 * results as JSONL.
 *
 * Targets the versioned ConnectRPC endpoints introduced in Sourcegraph 7.0:
 *
 *   POST /api/searchjobs.v1.Service/CreateSearchJob   (scope: externalapi:write)
 *   POST /api/searchjobs.v1.Service/GetSearchJob      (scope: externalapi:read)
 *   GET  <SearchJob.resultsUrl>                        -> JSONL
 *
 * Usage:
 *   export SRC_ENDPOINT="https://demo.sourcegraph.com"   # your instance
 *   export SRC_ACCESS_TOKEN="sgp_..."                    # externalapi:read + write
 *
 *   node search_job.ts
 *   node search_job.ts --query 'context:global patterntype:keyword TODO count:all' --out todos.jsonl
 *
 * Flags:
 *   --query <q>    search query (default: see DEFAULT_QUERY)
 *   --out <path>   output file, or "-" for stdout (default: results.jsonl)
 *   --poll <secs>  poll interval (default: 5)
 *   --timeout <s>  give up after this long (default: 1800)
 *   --quiet        no spinner or progress bar, just the result lines
 *
 * All status output goes to stderr, so `--out -` yields clean JSONL on stdout.
 * Colors and animation are disabled automatically when stderr is not a TTY;
 * NO_COLOR / FORCE_COLOR are honored.
 *
 * Runs on Node 22.18+ or 24.3+, where type stripping is on by default. No dependencies.
 * On Node 22.6-22.17:  node --experimental-strip-types search_job.ts
 * On anything older:   npx tsx search_job.ts
 */

import { createWriteStream } from "node:fs";
import { Readable } from "node:stream";
import { finished } from "node:stream/promises";
import { setTimeout as sleep } from "node:timers/promises";
import { styleText } from "node:util";

const DEFAULT_QUERY = "context:global patterntype:keyword TODO count:all";

/** Mirrors searchjobs.v1.SearchJob (JSON field names). */
interface SearchJob {
  name: string; // users/{user}/searchJobs/{id}
  query: string;
  state: string; // STATE_QUEUED | STATE_PROCESSING | STATE_COMPLETED | ...
  createTime?: string;
  startTime?: string;
  resultsUrl?: string;
  logsUrl?: string;
}

interface Options {
  query: string;
  out: string;
  pollMs: number;
  timeoutMs: number;
  quiet: boolean;
}

// ---------------------------------------------------------------------------
// Terminal UI. Everything here writes to stderr; stdout is reserved for JSONL.
// ---------------------------------------------------------------------------

type Style = Parameters<typeof styleText>[0];

/** styleText already no-ops on non-TTY streams and respects NO_COLOR. */
const paint = (style: Style, s: string) => styleText(style, s, { stream: process.stderr });

const c = {
  dim: (s: string) => paint("dim", s),
  bold: (s: string) => paint("bold", s),
  cyan: (s: string) => paint("cyan", s),
  green: (s: string) => paint("green", s),
  red: (s: string) => paint("red", s),
  yellow: (s: string) => paint("yellow", s),
};

const OK = c.green("✔");
const FAIL = c.red("✖");
const WARN = c.yellow("⚠");

const FRAMES = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
const CLEAR_LINE = "\r\x1b[2K";
const HIDE_CURSOR = "\x1b[?25l";
const SHOW_CURSOR = "\x1b[?25h";

let quiet = false;
const setQuiet = (v: boolean) => {
  quiet = v;
};
/** True when we may draw transient, self-overwriting lines. */
const live = (): boolean => Boolean(process.stderr.isTTY) && !quiet;

let cursorHidden = false;
function hideCursor(): void {
  if (!cursorHidden && live()) {
    process.stderr.write(HIDE_CURSOR);
    cursorHidden = true;
  }
}
function showCursor(): void {
  if (cursorHidden) {
    process.stderr.write(SHOW_CURSOR);
    cursorHidden = false;
  }
}
// A hidden cursor outlives the process, so restore it on every exit path.
process.on("exit", showCursor);
process.on("SIGINT", () => {
  showCursor();
  process.stderr.write("\n");
  process.exit(130);
});

const clamp = (n: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, n));

function fmtBytes(n: number): string {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${i === 0 ? Math.round(v) : v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

function fmtDuration(ms: number): string {
  if (ms < 10_000) return `${(ms / 1000).toFixed(1)}s`;
  const secs = Math.round(ms / 1000);
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ${secs % 60}s`;
  return `${Math.floor(mins / 60)}h ${mins % 60}m`;
}

const fmtCount = (n: number) => n.toLocaleString("en-US");

/** A one-line status indicator that animates while an indeterminate step runs. */
class Spinner {
  private timer?: NodeJS.Timeout;
  private frame = 0;
  private text: string;
  private stopped = false;
  private readonly startedAt = Date.now();

  constructor(text: string) {
    this.text = text;
    if (live()) {
      hideCursor();
      this.render();
      this.timer = setInterval(() => this.render(), 80);
      this.timer.unref();
    }
  }

  get elapsed(): number {
    return Date.now() - this.startedAt;
  }

  /** Swap the label; the interval picks it up on the next frame. */
  update(text: string): void {
    this.text = text;
  }

  /** Emit a permanent line above the spinner. */
  log(text: string): void {
    if (live()) process.stderr.write(CLEAR_LINE);
    process.stderr.write(`${text}\n`);
  }

  private render(): void {
    this.frame = (this.frame + 1) % FRAMES.length;
    process.stderr.write(`${CLEAR_LINE}${c.cyan(FRAMES[this.frame])} ${this.text}`);
  }

  stop(symbol: string, text: string): void {
    if (this.stopped) return;
    this.dispose();
    process.stderr.write(`${symbol} ${text}\n`);
  }

  /** Halt the animation and clear the line without printing a result. */
  dispose(): void {
    if (this.stopped) return;
    this.stopped = true;
    if (this.timer) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
    if (live()) {
      process.stderr.write(CLEAR_LINE);
      showCursor();
    }
  }
}

/**
 * Byte-oriented progress bar. Falls back to a byte/rate readout when the
 * server streams without a Content-Length, which is common for search results.
 */
class ProgressBar {
  private readonly startedAt = Date.now();
  private lastRenderAt = 0;
  private frame = 0;
  private value = 0;
  private readonly total?: number;

  // Declared explicitly rather than as a parameter property: Node's strip-only
  // TypeScript mode rejects those, and this file must run under bare `node`.
  constructor(total?: number) {
    this.total = total;
    if (live()) {
      hideCursor();
      this.render();
    }
  }

  /**
   * A multi-hundred-megabyte download yields tens of thousands of chunks, so
   * redraws are throttled to ~10/s rather than done per chunk.
   */
  update(value: number): void {
    this.value = value;
    if (!live()) return;
    const now = Date.now();
    if (now - this.lastRenderAt < 100) return;
    this.lastRenderAt = now;
    this.render();
  }

  private render(): void {
    process.stderr.write(CLEAR_LINE + this.line());
  }

  private line(): string {
    // An unsized pty reports 0 rather than undefined, so treat any
    // non-positive value as "unknown" and assume a conventional 80 columns.
    const reported = process.stderr.columns;
    const cols = Math.max(40, reported && reported > 0 ? reported : 80);
    const elapsed = Date.now() - this.startedAt;
    const rate = elapsed > 0 ? this.value / (elapsed / 1000) : 0;

    // [plainText, styledText] so widths can be measured without ANSI noise.
    const segs: Array<[string, string]> = [];
    const push = (plain: string, styled = plain) => segs.push([plain, styled]);

    if (this.total !== undefined) {
      const frac = clamp(this.value / this.total, 0, 1);
      const width = clamp(cols - 46, 10, 36);
      const filled = Math.round(frac * width);
      push(
        "█".repeat(filled) + "░".repeat(width - filled),
        c.cyan("█".repeat(filled)) + c.dim("░".repeat(width - filled)),
      );
      push(`${String(Math.round(frac * 100)).padStart(3)}%`);
      push(`${fmtBytes(this.value)} / ${fmtBytes(this.total)}`, c.dim(`${fmtBytes(this.value)} / ${fmtBytes(this.total)}`));
      push(`${fmtBytes(rate)}/s`, c.dim(`${fmtBytes(rate)}/s`));
      if (rate > 0 && frac < 1) {
        const eta = fmtDuration(((this.total - this.value) / rate) * 1000);
        push(`eta ${eta}`, c.dim(`eta ${eta}`));
      }
    } else {
      this.frame = (this.frame + 1) % FRAMES.length;
      push(FRAMES[this.frame], c.cyan(FRAMES[this.frame]));
      push(fmtBytes(this.value));
      push(`${fmtBytes(rate)}/s`, c.dim(`${fmtBytes(rate)}/s`));
      push(fmtDuration(elapsed), c.dim(fmtDuration(elapsed)));
    }

    // Drop trailing detail rather than wrapping onto a second line.
    const plainWidth = () => segs.reduce((n, [p]) => n + p.length, 0) + 2 * (segs.length - 1) + 2;
    while (segs.length > 2 && plainWidth() > cols - 1) segs.pop();

    return "  " + segs.map(([, styled]) => styled).join("  ");
  }

  stop(): void {
    if (live()) {
      process.stderr.write(CLEAR_LINE);
      showCursor();
    }
  }
}

// ---------------------------------------------------------------------------

function parseArgs(argv: string[]): Options {
  const opts: Options = {
    query: DEFAULT_QUERY,
    out: "results.jsonl",
    pollMs: 5_000,
    timeoutMs: 30 * 60_000,
    quiet: false,
  };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const next = () => {
      const v = argv[++i];
      if (v === undefined) throw new Error(`missing value for ${arg}`);
      return v;
    };
    switch (arg) {
      case "--query": opts.query = next(); break;
      case "--out": opts.out = next(); break;
      case "--poll": opts.pollMs = Number(next()) * 1000; break;
      case "--timeout": opts.timeoutMs = Number(next()) * 1000; break;
      case "--quiet": opts.quiet = true; break;
      default: throw new Error(`unknown flag: ${arg}`);
    }
  }
  return opts;
}

function authHeaders(token: string, json = false): Record<string, string> {
  const h: Record<string, string> = { Authorization: `token ${token}` };
  if (json) h["Content-Type"] = "application/json";
  return h;
}

async function apiError(res: Response): Promise<Error> {
  const text = await res.text();
  try {
    const j = JSON.parse(text) as { code?: string; message?: string };
    if (j.message) return new Error(`HTTP ${res.status}: ${j.message} (${j.code ?? ""})`);
  } catch {
    // fall through to raw body
  }
  return new Error(`HTTP ${res.status}: ${text.trim()}`);
}

async function rpc<T>(endpoint: string, token: string, method: string, body: unknown): Promise<T> {
  const res = await fetch(`${endpoint}/api/searchjobs.v1.Service/${method}`, {
    method: "POST",
    headers: authHeaders(token, true),
    body: JSON.stringify(body),
  });
  if (!res.ok) throw await apiError(res);
  return (await res.json()) as T;
}

function createSearchJob(endpoint: string, token: string, query: string): Promise<SearchJob> {
  // The caller only sets query; parent "users/-" is the authenticated user.
  return rpc<SearchJob>(endpoint, token, "CreateSearchJob", {
    parent: "users/-",
    searchJob: { query },
  });
}

function getSearchJob(endpoint: string, token: string, name: string): Promise<SearchJob> {
  return rpc<SearchJob>(endpoint, token, "GetSearchJob", { name });
}

const prettyState = (state: string) => state.replace(/^STATE_/, "").toLowerCase();

/** Poll GetSearchJob until the job reaches a terminal state. */
async function poll(endpoint: string, token: string, name: string, pollMs: number, timeoutMs: number): Promise<SearchJob> {
  const deadline = Date.now() + timeoutMs;
  const spinner = new Spinner("waiting for job");
  let last = "";

  const label = (state: string) => `${prettyState(state)} ${c.dim(fmtDuration(spinner.elapsed))}`;

  try {
    for (;;) {
      const job = await getSearchJob(endpoint, token, name);
      if (job.state !== last) {
        last = job.state;
        // Without a live spinner to carry the state, log transitions as before.
        if (!live()) spinner.log(`  state=${job.state}`);
      }
      spinner.update(label(job.state));

      if (job.state === "STATE_COMPLETED") {
        spinner.stop(OK, `Job completed in ${fmtDuration(spinner.elapsed)}`);
        return job;
      }
      if (job.state === "STATE_FAILED" || job.state === "STATE_CANCELED") {
        spinner.stop(FAIL, `Job ended in ${prettyState(job.state)} after ${fmtDuration(spinner.elapsed)}`);
        if (job.logsUrl) process.stderr.write(`  ${c.dim(`logs: ${job.logsUrl}`)}\n`);
        throw new Error(`job ended in ${job.state} (see logs: ${job.logsUrl ?? "n/a"})`);
      }
      // STATE_UNSPECIFIED | STATE_QUEUED | STATE_PROCESSING | STATE_ERRORED -> keep waiting.
      if (Date.now() > deadline) {
        spinner.stop(FAIL, `Timed out after ${fmtDuration(spinner.elapsed)}`);
        throw new Error(`timed out waiting for job (last state ${job.state})`);
      }

      // Keep the elapsed counter ticking between polls.
      spinner.update(label(job.state));
      await sleep(pollMs);
    }
  } finally {
    // No-op once stopped; keeps the animation from surviving an RPC throw.
    spinner.dispose();
  }
}

/** Stream the JSONL results URL to outPath, counting lines and bytes. */
async function downloadResults(
  endpoint: string,
  token: string,
  resultsUrl: string,
  outPath: string,
): Promise<{ lines: number; bytes: number }> {
  const full = new URL(resultsUrl, endpoint + "/").toString();
  const res = await fetch(full, { headers: authHeaders(token) });
  if (!res.ok) throw await apiError(res);
  if (!res.body) throw new Error("results response had no body");

  const declared = Number(res.headers.get("content-length"));
  const total = Number.isFinite(declared) && declared > 0 ? declared : undefined;

  const toFile = outPath !== "-";
  const sink = toFile ? createWriteStream(outPath) : process.stdout;

  if (live()) process.stderr.write(`${c.bold("Downloading results")}\n`);
  const bar = new ProgressBar(total);

  const decoder = new TextDecoder();
  let leftover = "";
  let lines = 0;
  let bytes = 0;

  try {
    for await (const chunk of Readable.fromWeb(res.body as any)) {
      sink.write(chunk);
      bytes += (chunk as Buffer).length;
      bar.update(bytes);
      leftover += decoder.decode(chunk as Buffer, { stream: true });
      let idx: number;
      while ((idx = leftover.indexOf("\n")) >= 0) {
        const line = leftover.slice(0, idx);
        leftover = leftover.slice(idx + 1);
        if (line.trim().length > 0) lines++;
      }
    }
    leftover += decoder.decode();
    if (leftover.trim().length > 0) lines++;
  } finally {
    bar.stop();
  }

  if (toFile) {
    sink.end();
    await finished(sink);
  }
  return { lines, bytes };
}

async function main(): Promise<void> {
  const opts = parseArgs(process.argv.slice(2));
  setQuiet(opts.quiet);

  const endpoint = (process.env.SRC_ENDPOINT ?? "https://demo.sourcegraph.com").replace(/\/+$/, "");
  const token = process.env.SRC_ACCESS_TOKEN;
  if (!token) {
    throw new Error("SRC_ACCESS_TOKEN is not set. Create a token with externalapi:read and externalapi:write scopes.");
  }

  const dest = opts.out === "-" ? "stdout" : opts.out;
  const row = (k: string, v: string) => process.stderr.write(`  ${c.dim(k.padEnd(8))}  ${v}\n`);
  process.stderr.write(`${c.bold("Sourcegraph search job")}\n`);
  row("endpoint", endpoint);
  row("query", c.cyan(opts.query));
  row("out", dest);
  process.stderr.write("\n");

  const creating = new Spinner("Creating search job");
  let created: SearchJob;
  try {
    created = await createSearchJob(endpoint, token, opts.query);
  } catch (err) {
    creating.stop(FAIL, "Failed to create search job");
    throw err;
  }
  creating.stop(OK, `Created ${c.dim(created.name)}`);

  const job = await poll(endpoint, token, created.name, opts.pollMs, opts.timeoutMs);

  if (!job.resultsUrl) throw new Error("job completed but no resultsUrl was returned");

  const startedAt = Date.now();
  const { lines, bytes } = await downloadResults(endpoint, token, job.resultsUrl, opts.out);
  const symbol = lines === 0 ? WARN : OK;
  process.stderr.write(`${symbol} Wrote ${c.bold(fmtCount(lines))} result line(s) to ${dest}\n`);
  process.stderr.write(`  ${c.dim(`${fmtBytes(bytes)} in ${fmtDuration(Date.now() - startedAt)}`)}\n`);
}

main().catch((err: unknown) => {
  showCursor();
  process.stderr.write(`${FAIL} ${c.red("error:")} ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});
