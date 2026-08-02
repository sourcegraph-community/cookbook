import test from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { analyze, classifyPath, extractMarkers, ownersFor, parseCodeowners, queriesForCount, scoreDebt, SEARCH_QUERIES } from "./todo_debt_radar.ts";

test("collection submits count:all queries with single RE2 escapes", () => {
  assert.ok(SEARCH_QUERIES.every(query => query.endsWith("count:all")));
  assert.match(SEARCH_QUERIES[0], /\[ \\t\]\*.*\\b count:all$/);
  assert.ok(!SEARCH_QUERIES[0].includes(String.raw`\s`));
  assert.match(SEARCH_QUERIES[1], / \.\+ count:all$/);
});

test("collection count can be limited or left exhaustive", () => {
  assert.ok(queriesForCount("5").every(query => query.endsWith("count:5")));
  assert.ok(queriesForCount("ALL").every(query => query.endsWith("count:all")));
  assert.throws(() => queriesForCount("0"), /positive integer/);
  assert.throws(() => queriesForCount("5.5"), /positive integer/);
});

test("extracts marker metadata but not ticket as author", () => {
  assert.deepEqual(extractMarkers("// TODO(alice) APP-42 #123")[0], { marker: "TODO", column: 3, author: "alice", tickets: ["APP-42", "#123"] });
  assert.equal(extractMarkers("# FIXME(APP-42)")[0].author, undefined);
});
test("CODEOWNERS wildcard, longest prefix, and later tie", () => {
  const rules = parseCodeowners("* @all\nsrc/** @src\nsrc/api* @first\nsrc/api* @later");
  assert.deepEqual(ownersFor("src/api.ts", rules), ["@later"]); assert.deepEqual(ownersFor("docs/a.md", rules), ["@all"]);
});
test("CODEOWNERS handles inline comments, ownerless overrides, and direct children", () => {
  const rules = parseCodeowners("* @all # fallback\n/apps/ @app\n/apps/github\ndocs/* @docs");
  assert.deepEqual(ownersFor("other.js", rules), ["@all"]);
  assert.deepEqual(ownersFor("apps/github/readme.md", rules), []);
  assert.deepEqual(ownersFor("docs/readme.md", rules), ["@docs"]);
  assert.deepEqual(ownersFor("docs/guides/readme.md", rules), ["@all"]);
});
test("path classes and score decomposition", () => {
  assert.equal(classifyPath("src/x.ts"), "production"); assert.equal(classifyPath("test/x.ts"), "test"); assert.equal(classifyPath("vendor/x.ts"), "vendored");
  assert.deepEqual(scoreDebt("FIXME", "production", "FIXME APP-42 2020-01", new Date("2025-02-01")), { marker: 4, path: 4, age: 5, ticketEra: 2, total: 15 });
});
test("analyze fixture streams enriched output and owner-filtered report", async () => {
  const dir = await mkdtemp(join(tmpdir(), "radar-")), out = join(dir, "out.jsonl");
  const report = await analyze(new URL("fixtures", import.meta.url).pathname, out, 5, "backend", "https://sourcegraph.test");
  const records = (await readFile(out, "utf8")).trim().split("\n").map(JSON.parse);
  assert.equal(records.length, 2); assert.deepEqual(records[0].owners, ["@backend", "@oncall"]); assert.equal(records[0].line, 11);
  assert.equal(records[0].column, 4); assert.match(records[0].url, /github\.com\/acme\/app@abc\/-\/blob\/src\/api\.ts\?L11$/);
  assert.match(report, /markers per affected file proxy/); assert.match(report, /APP-42/); assert.doesNotMatch(report, /## Unowned\n\n-/);
});
