import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { webcrypto } from "node:crypto";
import test from "node:test";
import { parse } from "@babel/parser";

const ui = readFileSync(new URL("../internal/plugin/ui.html", import.meta.url), "utf8");
const source = [...ui.matchAll(/<script>\s*([\s\S]*?)<\/script>/g)].at(-1)[1];
const declarations = parse(source).program.body.filter(node => node.type === "FunctionDeclaration");
const functions = declarations.filter(node => ["canResetAuthQuota", "resetAuthQuota"].includes(node.id.name));
assert.equal(functions.length, 2);

test("quota averages give each auth file equal weight and count available windows independently", async t => {
  const aggregate = declarations.find(node => node.id.name === "aggregateAuthQuota");
  assert.ok(aggregate);
  const context = vm.createContext({});
  vm.runInContext(source.slice(aggregate.start, aggregate.end), context);
  const row = (label, remaining_percent, extra = {}) => ({ label, remaining_percent, ...extra });
  const five = value => row("5-hour limit", value);
  const weekly = value => row("Weekly limit", value);
  const scenarios = [
    { name: "equal weight across subscription types, including zero and missing windows",
      files: [{ auth_index: "a", quota: [five(0), weekly(30)] }, { auth_index: "b", quota: [five(100)] }, { auth_index: "c" }],
      cached: [["a", { plan: "plus", quota: [five(0), weekly(30)] }], ["b", { plan: "pro-20x", quota: [five(100)] }]],
      expected: [[50, 2], [30, 1]] },
    { name: "refreshed data replaces snapshots, including empty responses",
      files: [{ auth_index: "a", quota: [five(80), weekly(100)] }, { auth_index: "b", quota: [five(100)] }],
      cached: [["a", { quota: [five(20), weekly(40)] }], ["b", { quota: [] }]],
      expected: [[20, 1], [40, 1]] },
    { name: "multiple provider groups keep one vote per auth file",
      files: [{ auth_index: "a", quota: [row("5-hour limit", 20, { group_label: "Gemini" }),
        row("5-hour limit", 80, { group_label: "Claude" })] }, { auth_index: "b", quota: [five(100)] }],
      expected: [[75, 2], [undefined, 0]] },
    { name: "main windows exclude model-specific and code-review limits",
      files: [{ auth_index: "a", quota: [five(60), weekly(40), row("Code review limit", 100),
        row("Spark 5-hour limit", 100, { label_prefix: "Spark ", label_message: { message_key: "backend.5_hour_limit" } }),
        row("Opus weekly limit", 100), row("Monthly limit", 100)] }],
      expected: [[60, 1], [40, 1]] },
    { name: "translation metadata identifies windows without relying on display language",
      files: [{ auth_index: "a", quota: [row("translated", 25, { label_message: { message_key: "backend.5_hour_limit" } }),
        row("translated", 75, { label_message: { message_key: "backend.weekly_limit" } })] }],
      expected: [[25, 1], [75, 1]] },
    { name: "invalid percentages are missing data",
      files: [{ auth_index: "a", quota: [null, undefined, "50", NaN, Infinity, -1, 101].map(five) }],
      expected: [[undefined, 0], [undefined, 0]] },
    { name: "loading and failed queries do not contribute stale data",
      files: [{ auth_index: "a", quota: [five(20)] }, { auth_index: "b", quota: [five(100), weekly(80)] },
        { auth_index: "c", quota: [five(100), weekly(90)] }], loading: ["b"], errors: [["c", "query failed"]],
      expected: [[20, 1], [undefined, 0]] },
    { name: "empty filtered list has no averages", files: [], expected: [[undefined, 0], [undefined, 0]] }
  ];
  for (const scenario of scenarios) {
    await t.test(scenario.name, () => {
      const owner = { authQuotas: new Map(scenario.cached), authQuotaLoading: new Set(scenario.loading),
        authQuotaErrors: new Map(scenario.errors) };
      const actual = context.aggregateAuthQuota(scenario.files, owner);
      assert.deepEqual(Array.from(actual, window => [window.average, window.count]), scenario.expected);
    });
  }
});

test("quota reset redeems once, releases the core cooldown, and refreshes independently", async t => {
  for (const scenario of ["admin", "account", "provider failure", "core failure", "core forbidden", "invalid core response", "refresh failure"]) {
    await t.test(scenario, async () => {
      const account = scenario === "account";
      const file = { auth_index: "codex-1", name: "dummy.json", category: "codex", quota_supported: true,
        disabled: false, unavailable: true, rate_limit_reset_credits_available_count: 1 };
      const owner = { authQuotas: new Map(), authQuotaLoading: new Set(), authQuotaErrors: new Map() };
      const calls = [], notifications = [];
      const redeem = async (path, opts) => {
        calls.push("redeem");
        assert.match(path, /^\/auth-files\/quota\/reset\?/);
        assert.match(opts.headers["X-Quota-Reset-ID"], /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
        if (scenario === "provider failure") throw new Error("provider unavailable");
        return { reset: true };
      };
      const context = vm.createContext({
        currentRole: account ? "account" : "admin", accountUIState: owner, adminUIState: owner,
        resources: { account: { profile: { value: { can_reset_auth_quota: true } } } },
        authQuotaGeneration: { admin: 0, account: 0 }, AUTH_QUOTA_TIMEOUT_MS: 65000,
        crypto: webcrypto, Uint8Array, URLSearchParams, UIError: Error, m: key => key, int: String,
        authQuotaCacheCurrent: () => true, persistAuthQuotaCache: () => {}, renderAuthFiles: () => {},
        openActionDialog: async (_title, _message, _unused, action) => action(),
        plugin: async (method, path, body, opts) => {
          assert.equal(method, "POST");
          return redeem(path, opts);
        },
        accountAPI: redeem,
        api: async (method, path, body, opts) => {
          calls.push("core");
          assert.equal(method, "POST");
          assert.equal(path, "/v0/management/reset-quota");
          assert.equal(body.auth_index, file.auth_index);
          assert.equal(opts.keepSessionOnForbidden, true);
          if (scenario === "core failure" || scenario === "core forbidden") throw new Error("core unavailable");
          if (scenario === "invalid core response") return { status: "ok", auth_index: "another-file" };
          return { status: "ok", auth_index: file.auth_index };
        },
        refreshAuthQuota: async () => {
          calls.push("refresh");
          if (scenario === "refresh failure") owner.authQuotaErrors.set(file.auth_index, "query failed");
        },
        notify: (message, kind) => notifications.push({ message, kind })
      });
      vm.runInContext(functions.map(node => source.slice(node.start, node.end)).join("\n"), context);
      if (scenario === "provider failure") {
        await assert.rejects(context.resetAuthQuota(file, account), /provider unavailable/);
        assert.deepEqual(calls, ["redeem"]);
        assert.equal(notifications.length, 0);
      } else {
        await context.resetAuthQuota(file, account);
        assert.deepEqual(calls, account ? ["redeem", "refresh"] : ["redeem", "core", "refresh"]);
        const coreFailed = ["core failure", "core forbidden", "invalid core response"].includes(scenario);
        assert.equal(file.unavailable, account || coreFailed);
        assert.equal(notifications[0].message, coreFailed ? "ui.quota_reset_but_core_cooldown_failed" :
          scenario === "refresh failure" ? "ui.quota_reset_but_refresh_failed_please_update_the_quota_manually" : "ui.reset_codex_quota_for_value");
        assert.equal(notifications[0].kind, coreFailed || scenario === "refresh failure" ? "err" : "ok");
      }
      assert.equal(owner.authQuotaLoading.size, 0);
    });
  }
});
