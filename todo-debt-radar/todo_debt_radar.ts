/** Exhaustive TODO debt inventory using Sourcegraph Search Jobs. No dependencies. */
import { createReadStream, createWriteStream } from "node:fs";
import { mkdir } from "node:fs/promises";
import { once } from "node:events";
import { dirname } from "node:path";
import { finished } from "node:stream/promises";
import { Readable } from "node:stream";
import { createInterface } from "node:readline";
import { pathToFileURL } from "node:url";

export const DEBT_QUERY = String.raw`context:global patterntype:regexp (//|#|/\*|\*|<!--|--|;)\s*(TODO|FIXME|HACK|XXX)\b count:all`;
export const OWNERS_QUERY = String.raw`context:global patterntype:regexp file:^(CODEOWNERS|\.github/CODEOWNERS|docs/CODEOWNERS)$ . count:all`;

type Pos = { line: number; column: number; offset?: number };
export const SEARCH_QUERIES = [DEBT_QUERY, OWNERS_QUERY]
  .map(query => query.replaceAll("\\\\", "\\"))
  .map(query => query.replace(String.raw`\s*`, String.raw`[ \t]*`).replace(" . count:all", " .+ count:all"));

export function queriesForCount(value: string): string[] {
  const count = value.toLowerCase();
  if (count !== "all" && !/^[1-9]\d*$/.test(count)) throw new Error("--count must be 'all' or a positive integer");
  return SEARCH_QUERIES.map(query => query.replace(/count:all$/, `count:${count}`));
}

type Range = { start: Pos; end: Pos };
type Chunk = { content: string; contentStart: Pos; ranges: Range[]; bestLineMatch?: number };
type Content = { type: "content"; repository: string; path: string; commit?: string; chunkMatches: Chunk[] };
export type OwnerRule = { pattern: string; owners: string[]; line: number; prefix: number };
export type Score = { marker: number; path: number; age: number; ticketEra: number; total: number };
export type Debt = { repository: string; path: string; commit?: string; line: number; column: number; text: string; marker: string; author?: string; tickets: string[]; pathClass: string; owners: string[]; url: string; score: Score };

const value = (args: string[], name: string, fallback: string) => {
  const i = args.indexOf(name);
  if (i < 0) return fallback;
  const result = args[i + 1];
  if (result === undefined || result.startsWith("--")) throw new Error(`missing value for ${name}`);
  return result;
};
const normalizeOwner = (s: string) => s.replace(/^@/, "").toLowerCase();
const DEFAULT_ENDPOINT = (process.env.SRC_ENDPOINT ?? "https://demo.sourcegraph.com").replace(/\/$/, "");
const positiveNumber = (value: string, flag: string) => {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed <= 0) throw new Error(`${flag} must be a positive number`);
  return parsed;
};

/** Return only range-identified file lines; a line hit by several ranges appears once. */
export function rangedLines(hit: Content): { line: number; column: number; text: string }[] {
  const found = new Map<number, { line: number; column: number; text: string }>();
  for (const chunk of hit.chunkMatches ?? []) {
    const lines = chunk.content.split(/\r?\n/);
    for (const range of chunk.ranges ?? []) for (let n = range.start.line; n <= range.end.line; n++) {
      const local = n - chunk.contentStart.line;
      if (local < 0 || local >= lines.length) continue;
      const column = n === range.start.line ? range.start.column : 0;
      const old = found.get(n);
      if (!old || column < old.column) found.set(n, { line: n + 1, column, text: lines[local] });
    }
  }
  return [...found.values()];
}

export function extractMarkers(text: string) {
  const found: { marker: string; column: number; author?: string; tickets: string[] }[] = [];
  const seen = new Set<string>();
  const re = /\b(TODO|FIXME|HACK|XXX)\b(?:\(([^)]+)\))?/gi;
  for (const m of text.matchAll(re)) {
    const marker = m[1].toUpperCase(), paren = m[2]?.trim();
    const tickets = [...new Set([...text.matchAll(/\b[A-Z][A-Z0-9]+-\d+\b|(?<!\w)#\d+\b/g)].map(x => x[0]))];
    const author = paren && !/^[A-Z][A-Z0-9]+-\d+$/i.test(paren) && !/^#\d+$/.test(paren) && /^[\w .@-]+$/.test(paren) ? paren : undefined;
    const key = `${marker}:${m.index}`;
    if (!seen.has(key)) { seen.add(key); found.push({ marker, column: m.index ?? 0, author, tickets }); }
  }
  return found;
}

export function classifyPath(path: string): string {
  const p = path.toLowerCase();
  if (/(^|\/)(vendor|vendors|third_party|node_modules|deps)(\/|$)/.test(p)) return "vendored";
  if (/(^|\/)(generated|gen)(\/|$)|\.generated\./.test(p)) return "generated";
  if (/(^|\/)(test|tests|spec|specs|__tests__)(\/|$)|\.(test|spec)\./.test(p)) return "test";
  if (/(^|\/)(doc|docs|documentation)(\/|$)|\.(md|rst|adoc)$/.test(p)) return "docs";
  return "production";
}

export function parseCodeowners(text: string): OwnerRule[] {
  const rules: OwnerRule[] = [];
  text.split(/\r?\n/).forEach((raw, i) => {
    const line = raw.replace(/\s+#.*$/, "").trim(); if (!line || line.startsWith("#")) return;
    const fields = line.split(/\s+/);
    const pattern = fields[0], wildcard = pattern.search(/[?*[]/);
    rules.push({ pattern, owners: fields.slice(1), line: i + 1, prefix: (wildcard < 0 ? pattern : pattern.slice(0, wildcard)).replace(/^\//, "").length });
  });
  return rules;
}

function globRegex(pattern: string): RegExp {
  let p = pattern.replace(/^\//, ""), s = "";
  for (let i = 0; i < p.length; i++) {
    const c = p[i];
    if (c === "*") { if (p[i + 1] === "*") { i++; if (p[i + 1] === "/") { i++; s += "(?:.*/)?"; } else s += ".*"; } else s += "[^/]*"; }
    else if (c === "?") s += "[^/]";
    else if (c === "[") { const end = p.indexOf("]", i); if (end >= 0) { s += p.slice(i, end + 1); i = end; } else s += "\\["; }
    else s += c.replace(/[.+^${}()|\\]/g, "\\$&");
  }
  if (!p.includes("/")) s = "(?:^|.*/)" + s;
  const suffix = p.endsWith("/") ? ".*" : p.endsWith("/*") ? "$" : "(?:$|/.*)$";
  return new RegExp(`^${s}${suffix}`);
}

export function ownersFor(path: string, rules: OwnerRule[]): string[] {
  let best: OwnerRule | undefined;
  for (const rule of rules) if (globRegex(rule.pattern).test(path) && (!best || rule.prefix > best.prefix || (rule.prefix === best.prefix && rule.line > best.line))) best = rule;
  return best?.owners ?? [];
}

export function scoreDebt(marker: string, pathClass: string, text: string, now = new Date()): Score {
  const markerScore: Record<string, number> = { TODO: 2, FIXME: 4, HACK: 5, XXX: 5 };
  const pathScore: Record<string, number> = { production: 4, test: 2, docs: 1, generated: 0, vendored: 0 };
  let age = 0, ticketEra = 0;
  const dm = text.match(/\b(20\d\d)[-/](0?[1-9]|1[0-2])(?:[-/](?:0?[1-9]|[12]\d|3[01]))?\b/);
  if (dm) age = Math.max(0, Math.min(5, Math.floor((now.getTime() - new Date(Number(dm[1]), Number(dm[2]) - 1, 1).getTime()) / 31557600000)));
  const nums = [...text.matchAll(/\b[A-Z][A-Z0-9]+-(\d+)\b|(?<!\w)#(\d+)\b/g)].map(m => Number(m[1] ?? m[2]));
  if (nums.some(n => n > 0 && n < 1000)) ticketEra = 2;
  const score = { marker: markerScore[marker] ?? 0, path: pathScore[pathClass] ?? 0, age, ticketEra, total: 0 };
  score.total = score.marker + score.path + score.age + score.ticketEra; return score;
}

function encodePath(path: string): string {
  return path.split("/").map(encodeURIComponent).join("/");
}

function sourcegraphLink(endpoint: string, d: { repository: string; path: string; commit?: string; line: number }) {
  return `${endpoint.replace(/\/$/, "")}/${encodePath(d.repository)}@${encodeURIComponent(d.commit ?? "HEAD")}/-/blob/${encodePath(d.path)}?L${d.line}`;
}
const md = (s: string) => s.replace(/\\/g, "\\\\").replace(/([|`*_<>])/g, "\\$1").replace(/\r?\n/g, " ");

async function jsonLines(path: string, consume: (x: Content) => void | Promise<void>) {
  const rl = createInterface({ input: createReadStream(path), crlfDelay: Infinity });
  for await (const line of rl) if (line.trim()) await consume(JSON.parse(line) as Content);
}

export async function analyze(rawDir: string, outPath: string, topN: number, ownerFilter?: string, endpoint = DEFAULT_ENDPOINT) {
  const ownerFiles = new Map<string, { repository: string; path: string; rank: number; lines: Map<number, string> }>();
  await jsonLines(`${rawDir}/codeowners.jsonl`, hit => {
    if (hit.type !== "content") return;
    const rank = hit.path === ".github/CODEOWNERS" ? 3 : hit.path === "CODEOWNERS" ? 2 : hit.path === "docs/CODEOWNERS" ? 1 : 0;
    const key = `${hit.repository}\0${hit.path}`;
    const file = ownerFiles.get(key) ?? { repository: hit.repository, path: hit.path, rank, lines: new Map<number, string>() };
    for (const line of rangedLines(hit)) file.lines.set(line.line, line.text);
    ownerFiles.set(key, file);
  });
  const selected = new Map<string, { rank: number; text: string }>();
  for (const file of ownerFiles.values()) {
    const old = selected.get(file.repository); if (old && old.rank > file.rank) continue;
    const text = [...file.lines].sort((a, b) => a[0] - b[0]).map(([, line]) => line).join("\n");
    selected.set(file.repository, { rank: file.rank, text });
  }
  const rules = new Map([...selected].map(([repo, x]) => [repo, parseCodeowners(x.text)]));
  await mkdir(dirname(outPath), { recursive: true });
  const sink = createWriteStream(outPath); let total = 0;
  const top: Debt[] = [], repos = new Map<string, { count: number; files: Set<string> }>(), owners = new Map<string, number>(), markers = new Map<string, number>(); const unowned: Debt[] = [];
  await jsonLines(`${rawDir}/debt.jsonl`, async hit => {
    if (hit.type !== "content") return;
    for (const line of rangedLines(hit)) for (const mark of extractMarkers(line.text.slice(line.column))) {
      const column = line.column + mark.column + 1;
      const os = ownersFor(hit.path, rules.get(hit.repository) ?? []);
      if (ownerFilter && !os.some(o => normalizeOwner(o) === normalizeOwner(ownerFilter))) continue;
      const pathClass = classifyPath(hit.path);
      const d: Debt = { repository: hit.repository, path: hit.path, commit: hit.commit, line: line.line, column, text: line.text.trim(), marker: mark.marker, author: mark.author, tickets: mark.tickets, pathClass, owners: os, url: sourcegraphLink(endpoint, { ...hit, line: line.line }), score: scoreDebt(mark.marker, pathClass, line.text) };
      if (!sink.write(JSON.stringify(d) + "\n")) await once(sink, "drain");
      total++;
      const rr = repos.get(d.repository) ?? { count: 0, files: new Set<string>() }; rr.count++; rr.files.add(d.path); repos.set(d.repository, rr);
      markers.set(d.marker, (markers.get(d.marker) ?? 0) + 1);
      if (!os.length) { unowned.push(d); unowned.sort((a, b) => b.score.total - a.score.total); if (unowned.length > topN) unowned.pop(); }
      for (const o of os.length ? os : ["Unowned"]) owners.set(o, (owners.get(o) ?? 0) + 1);
      top.push(d); top.sort((a, b) => b.score.total - a.score.total); if (top.length > topN) top.pop();
    }
  });
  sink.end(); await finished(sink);
  const lines = [`# TODO debt radar`, ``, `**${total} markers**${ownerFilter ? ` owned by **${md(ownerFilter)}**` : ""}. Full enriched records: \`${md(outPath)}\`.`, ``, `## Top ${Math.min(topN, total)}`, ``, `| Score | Marker | Context | Location | Owners |`, `|---:|---|---|---|---|`];
  for (const d of top) lines.push(`| ${d.score.total} | ${d.marker} | ${md(d.text)} | [${md(d.repository + "/" + d.path + ":" + d.line)}](${d.url}) | ${d.owners.length ? d.owners.map(md).join(", ") : "Unowned"} |`);
  lines.push("", "## Repositories (markers per affected file proxy)", "", "| Repository | Markers | Files | Proxy |", "|---|---:|---:|---:|");
  for (const [r, x] of [...repos].sort((a,b) => b[1].count / b[1].files.size - a[1].count / a[1].files.size)) lines.push(`| ${md(r)} | ${x.count} | ${x.files.size} | ${(x.count/x.files.size).toFixed(2)} |`);
  lines.push("", "## Owners", "", "| Owner | Markers |", "|---|---:|"); for (const [o,n] of [...owners].sort((a,b)=>b[1]-a[1])) lines.push(`| ${md(o)} | ${n} |`);
  lines.push("", "## Marker types", "", "| Marker | Count |", "|---|---:|"); for (const [marker,n] of [...markers].sort((a,b)=>b[1]-a[1])) lines.push(`| ${marker} | ${n} |`);
  lines.push("", "## Unowned", ""); if (!unowned.length) lines.push("None."); else for (const d of unowned) lines.push(`- [${md(d.repository + "/" + d.path + ":" + d.line)}](${d.url}) — ${md(d.text)}`);
  return lines.join("\n") + "\n";
}

type Job = { name: string; state: string; resultsUrl?: string; logsUrl?: string };
async function collect(rawDir: string, pollMs: number, timeoutMs: number, count: string) {
  const queries = queriesForCount(count);
  const endpoint = DEFAULT_ENDPOINT, token = process.env.SRC_ACCESS_TOKEN;
  if (!token) throw new Error("SRC_ACCESS_TOKEN is not set (externalapi:read and externalapi:write required)");
  await mkdir(rawDir, { recursive: true }); const headers = { Authorization: `token ${token}` };
  const rpc = async (method: string, body: unknown) => { const r = await fetch(`${endpoint}/api/searchjobs.v1.Service/${method}`, { method: "POST", headers: { ...headers, "Content-Type": "application/json" }, body: JSON.stringify(body) }); if (!r.ok) throw new Error(`${method}: HTTP ${r.status}: ${await r.text()}`); return await r.json() as Job; };
  for (const [name, query] of [["debt", queries[0]], ["codeowners", queries[1]]]) {
    let job = await rpc("CreateSearchJob", { parent: "users/-", searchJob: { query } }); console.error(`created ${name}: ${job.name}`); const deadline = Date.now() + timeoutMs;
    while (job.state !== "STATE_COMPLETED") { if (["STATE_FAILED", "STATE_CANCELED"].includes(job.state)) throw new Error(`${name} ended ${job.state} (${job.logsUrl ?? "no logs URL"})`); if (Date.now() > deadline) throw new Error(`${name} timed out`); await new Promise(r => setTimeout(r, pollMs)); job = await rpc("GetSearchJob", { name: job.name }); console.error(`  ${name}: ${job.state}`); }
    if (!job.resultsUrl) throw new Error(`${name} completed without resultsUrl`); const response = await fetch(new URL(job.resultsUrl, endpoint + "/"), { headers }); if (!response.ok || !response.body) throw new Error(`${name} download failed: HTTP ${response.status}`);
    const sink = createWriteStream(`${rawDir}/${name}.jsonl`); for await (const chunk of Readable.fromWeb(response.body as any)) if (!sink.write(chunk)) await once(sink, "drain"); sink.end(); await finished(sink); console.error(`saved ${rawDir}/${name}.jsonl`);
  }
}

async function main() {
  const args = process.argv.slice(2), phase = !args[0] || args[0].startsWith("--") ? "all" : args.shift()!;
  if (!["all", "collect", "analyze"].includes(phase)) throw new Error("usage: todo_debt_radar.ts [collect|analyze] [flags]");
  const raw = value(args, "--raw-dir", "todo-debt-raw"), out = value(args, "--out", "todo-debt.jsonl"), top = positiveNumber(value(args, "--top", "20"), "--top");
  if (phase !== "analyze") await collect(raw, positiveNumber(value(args, "--poll", "5"), "--poll") * 1000, positiveNumber(value(args, "--timeout", "1800"), "--timeout") * 1000, value(args, "--count", "all"));
  if (phase !== "collect") process.stdout.write(await analyze(raw, out, top, value(args, "--owner", "") || undefined));
}
if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) main().catch(e => { console.error(e instanceof Error ? e.message : e); process.exitCode = 1; });
