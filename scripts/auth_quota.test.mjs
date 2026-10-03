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
