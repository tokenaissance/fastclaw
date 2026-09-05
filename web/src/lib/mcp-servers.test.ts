import assert from "node:assert/strict";
import { describe, test } from "node:test";
import type { MCPServerConfig } from "./api";
import { mergeMCPServersForSave } from "./mcp-servers";

const http = (url: string): MCPServerConfig => ({ type: "http", url });

describe("mergeMCPServersForSave", () => {
  test("preserves a server added concurrently since the page last fetched", () => {
    const baseline = { a: http("https://a.example/mcp") };
    const next = { a: http("https://a.example/mcp"), b: http("https://b.example/mcp") };
    const latest = {
      a: http("https://a.example/mcp"),
      b: http("https://b.example/mcp"),
      concurrent: http("https://concurrent.example/mcp"),
    };
    const merged = mergeMCPServersForSave(baseline, next, latest);
    assert.deepEqual(Object.keys(merged).sort(), ["a", "b", "concurrent"]);
  });

  test("does not resurrect a server the user deleted from the baseline", () => {
    const baseline = { a: http("https://a.example/mcp"), stale: http("https://stale.example/mcp") };
    const next = { a: http("https://a.example/mcp") };
    const latest = {
      a: http("https://a.example/mcp"),
      stale: http("https://stale.example/mcp"), // still server-side until this PATCH lands
    };
    const merged = mergeMCPServersForSave(baseline, next, latest);
    assert.deepEqual(Object.keys(merged), ["a"]);
  });

  test("user edits win over the latest value for a name they edited", () => {
    const baseline = { a: http("https://old.example/mcp") };
    const next = { a: http("https://edited.example/mcp") };
    const latest = { a: http("https://someone-else.example/mcp") };
    const merged = mergeMCPServersForSave(baseline, next, latest);
    assert.equal(merged.a.url, "https://edited.example/mcp");
  });

  test("rename keeps a concurrent add on the new name", () => {
    const baseline = { old: http("https://old.example/mcp") };
    const next = { fresh: http("https://fresh.example/mcp") };
    const latest = {
      old: http("https://old.example/mcp"),
      fresh: http("https://fresh.example/mcp"),
      concurrent: http("https://concurrent.example/mcp"),
    };
    const merged = mergeMCPServersForSave(baseline, next, latest);
    assert.deepEqual(Object.keys(merged).sort(), ["concurrent", "fresh"]);
  });
});
