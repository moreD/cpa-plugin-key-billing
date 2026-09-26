#!/usr/bin/env python3
"""Migrate the old CPA limits and scheduler settings to cpa-key-billing.

The old configuration stored per-client limits under api-keys[].cost-limits
and installed smart-load-balancer as a separate CPA plugin.  The new plugin
owns the scheduler and stores quotas in its own state database.

This command is deliberately offline by default.  It writes a migrated CPA
config and a JSON plan manifest.  Pass --apply after restarting CPA to sync the
keys and create/update the plans through the plugin management API.
"""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import shutil
import sys
import urllib.error
import urllib.request
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any

try:
    import yaml
except ImportError as exc:  # pragma: no cover - exercised by operator, not unit tests
    raise SystemExit("PyYAML is required: python3 -m pip install pyyaml") from exc


CALLER_SCOPE_SALT = b"cli-proxy-api:caller-scope:v1\0"
SEVEN_DAYS = 7 * 24 * 60 * 60
SMART_REGISTRY_MARKER = "nitansde/smart-load-balancer"


def caller_scope(api_key: str) -> str:
    return hashlib.sha256(CALLER_SCOPE_SALT + api_key.strip().encode()).hexdigest()


def amount(value: Any, key_name: str) -> Decimal:
    try:
        parsed = Decimal(str(value))
    except (InvalidOperation, ValueError) as exc:
        raise ValueError(f"API key {key_name!r} has an invalid 7d limit") from exc
    if not parsed.is_finite() or parsed <= 0:
        raise ValueError(f"API key {key_name!r} has a non-positive 7d limit")
    return parsed


def plan_id(limit: Decimal) -> str:
    cents = int((limit * 100).to_integral_value())
    normalized = format(limit, "f").replace(".", "_").replace("-", "minus_")
    return f"legacy-7d-{cents}-{normalized}"[:96].rstrip("_")


def parse_config(path: Path) -> dict[str, Any]:
    try:
        raw = yaml.safe_load(path.read_text())
    except OSError as exc:
        raise SystemExit(f"read {path}: {exc}") from exc
    if not isinstance(raw, dict):
        raise SystemExit(f"{path} must contain a YAML mapping")
    return raw


def migrate(raw: dict[str, Any], source: Path) -> tuple[dict[str, Any], dict[str, Any]]:
    output = copy.deepcopy(raw)
    plugins = output.setdefault("plugins", {})
    if not isinstance(plugins, dict):
        raise ValueError("plugins must be a YAML mapping")
    configs = plugins.setdefault("configs", {})
    if not isinstance(configs, dict):
        raise ValueError("plugins.configs must be a YAML mapping")

    legacy_smart = configs.pop("smart-load-balancer", None)
    scheduler_mode = "smart" if legacy_smart is not None else "disabled"
    cpa_config = configs.get("cpa-key-billing", {})
    if not isinstance(cpa_config, dict):
        raise ValueError("plugins.configs.cpa-key-billing must be a YAML mapping")
    cpa_config = copy.deepcopy(cpa_config)
    cpa_config.update({"enabled": True, "scheduler_mode": scheduler_mode})
    cpa_config.setdefault("state_file", "plugins/cpa-key-billing-state-v1.db")
    configs["cpa-key-billing"] = cpa_config
    plugins["enabled"] = True

    sources = plugins.get("store-sources")
    if isinstance(sources, list):
        kept = [item for item in sources if SMART_REGISTRY_MARKER not in str(item)]
        if kept:
            plugins["store-sources"] = kept
        else:
            plugins.pop("store-sources", None)

    grouped: dict[Decimal, list[str]] = {}
    key_rows: list[dict[str, str]] = []
    ignored: list[dict[str, str]] = []
    normalized_api_keys: list[str] = []
    api_keys = output.get("api-keys", [])
    if not isinstance(api_keys, list):
        raise ValueError("api-keys must be a YAML list")
    seen_scopes: set[str] = set()
    for index, entry in enumerate(api_keys, 1):
        if isinstance(entry, str) and entry.strip():
            normalized_api_keys.append(entry.strip())
            continue
        if not isinstance(entry, dict):
            continue
        key_name = str(entry.get("name") or f"key-{index}")
        raw_key = entry.get("api-key")
        if isinstance(raw_key, str) and raw_key.strip():
            normalized_api_keys.append(raw_key.strip())
        limits = entry.get("cost-limits")
        if isinstance(limits, dict) and "12h" in limits:
            ignored.append({"key": key_name, "window": "12h"})
        if isinstance(raw_key, str) and raw_key.strip() and isinstance(limits, dict) and "7d" in limits:
            scope = caller_scope(raw_key)
            if scope in seen_scopes:
                raise ValueError(f"duplicate API key scope for {key_name!r}")
            seen_scopes.add(scope)
            limit = amount(limits["7d"], key_name)
            grouped.setdefault(limit, []).append(scope)
            key_rows.append({"name": key_name, "scope": scope, "plan_id": plan_id(limit), "label": key_name})
        # Stock CPA does not know the legacy custom field. The plugin manifest
        # is the authoritative replacement, so remove both 12h and 7d fields.
        entry.pop("cost-limits", None)

    # Current stock CPA accepts api-keys as strings. The previous modified CPA
    # also accepted mapping entries with name/cost-limits, so normalize those
    # mappings while preserving their names in the plugin manifest.
    output["api-keys"] = normalized_api_keys

    plans = []
    for limit in sorted(grouped):
        identifier = plan_id(limit)
        plans.append({
            "id": identifier,
            "name": f"Legacy 7-day quota ${limit}",
            "windows": [{
                "id": "seven-day",
                "name": "7-day quota",
                "period_seconds": SEVEN_DAYS,
                "amount_usd": float(limit),
            }],
            "scopes": grouped[limit],
        })

    if legacy_smart is None:
        scheduler_note = "No legacy smart-load-balancer config was found; scheduler_mode remains disabled."
    else:
        scheduler_note = "The separate smart-load-balancer plugin config was removed; its 24h sticky and 8-request defaults are embedded."
    manifest = {
        "format": 1,
        "source_config": str(source),
        "scheduler_mode": scheduler_mode,
        "scheduler_note": scheduler_note,
        "ignored_limits": ignored,
        "plans": plans,
        "keys": key_rows,
    }
    return output, manifest


class ManagementAPI:
    def __init__(self, base_url: str, secret: str):
        self.base_url = base_url.rstrip("/")
        self.secret = secret

    def request(self, method: str, path: str, payload: dict[str, Any]) -> dict[str, Any]:
        body = json.dumps(payload).encode()
        request = urllib.request.Request(
            self.base_url + path,
            data=None if method == "GET" else body,
            method=method,
            headers={"Authorization": f"Bearer {self.secret}", "Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                decoded = json.loads(response.read())
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", "replace")[:500]
            raise RuntimeError(f"management API {method} {path} failed: HTTP {exc.code}: {detail}") from exc
        except (OSError, json.JSONDecodeError) as exc:
            raise RuntimeError(f"management API {method} {path} failed: {exc}") from exc
        if not isinstance(decoded, dict) or "error" in decoded:
            raise RuntimeError(f"management API {method} {path} returned an error")
        return decoded


def apply_manifest(manifest: dict[str, Any], api: ManagementAPI, keys: list[str]) -> None:
    api.request("POST", "/v0/management/plugins/cpa-key-billing/keys/sync", {"keys": keys, "allow_empty": True})
    existing_raw = api.request("GET", "/v0/management/plugins/cpa-key-billing/plans", {})
    existing = {
        item.get("id"): item
        for item in existing_raw.get("plans", [])
        if isinstance(item, dict) and item.get("id")
    }
    for plan in manifest["plans"]:
        if plan["id"] in existing:
            update = copy.deepcopy(plan)
            old_windows = existing[plan["id"]].get("windows", [])
            for window in update.get("windows", []):
                match = next(
                    (old for old in old_windows if old.get("period_seconds") == window.get("period_seconds")),
                    None,
                )
                if match and match.get("id"):
                    window["id"] = match["id"]
            api.request("PATCH", "/v0/management/plugins/cpa-key-billing/plans", update)
        else:
            create = copy.deepcopy(plan)
            for window in create.get("windows", []):
                window.pop("id", None)
            api.request("POST", "/v0/management/plugins/cpa-key-billing/plans", create)
    for entry in manifest.get("keys", []):
        if entry.get("label"):
            api.request("POST", "/v0/management/plugins/cpa-key-billing/keys/label", {
                "scope": entry["scope"],
                "label": entry["label"],
            })


def configured_api_keys(raw: Any) -> list[str]:
    if not isinstance(raw, list):
        return []
    values: list[str] = []
    for entry in raw:
        value = entry if isinstance(entry, str) else entry.get("api-key") if isinstance(entry, dict) else None
        if isinstance(value, str) and value.strip():
            values.append(value.strip())
    return values


def write_yaml(path: Path, value: dict[str, Any]) -> None:
    path.write_text(yaml.safe_dump(value, sort_keys=False, allow_unicode=True, default_flow_style=False))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=Path("config.yaml"), help="legacy CPA config (default: config.yaml)")
    parser.add_argument("--output", type=Path, help="migrated config (default: <config>.migrated.yaml)")
    parser.add_argument("--manifest", type=Path, help="plan manifest JSON (default: <config>.migration.json)")
    parser.add_argument("--in-place", action="store_true", help="replace --config after making a timestamped backup")
    parser.add_argument("--apply", action="store_true", help="sync keys and plans through the running plugin")
    parser.add_argument("--management-url", default="http://127.0.0.1:8088")
    parser.add_argument("--management-key", default=os.environ.get("CPA_MANAGEMENT_KEY"))
    args = parser.parse_args()
    if args.apply and args.manifest is None:
        parser.error("--apply requires the --manifest produced by the migration preview")
    if args.in_place:
        output_path = args.config
    elif args.apply:
        output_path = None
    else:
        output_path = args.output or args.config.with_name(args.config.name + ".migrated.yaml")
    manifest_path = args.manifest or args.config.with_name(args.config.name + ".migration.json")

    raw = parse_config(args.config)
    try:
        migrated, manifest = migrate(raw, args.config)
    except ValueError as exc:
        parser.error(str(exc))
    if args.apply:
        try:
            loaded_manifest = json.loads(manifest_path.read_text())
        except (OSError, json.JSONDecodeError) as exc:
            parser.error(f"read migration manifest {manifest_path}: {exc}")
        if not isinstance(loaded_manifest, dict) or loaded_manifest.get("format") != 1:
            parser.error(f"invalid migration manifest: {manifest_path}")
        manifest = loaded_manifest

    if args.in_place:
        backup = args.config.with_name(args.config.name + ".pre-cpa-key-billing-migration")
        shutil.copy2(args.config, backup)
        print(f"backup: {backup}")
    if output_path is not None:
        write_yaml(output_path, migrated)
    if not args.apply:
        manifest_path.write_text(json.dumps(manifest, indent=2, ensure_ascii=False) + "\n")
    if output_path is not None:
        print(f"config: {output_path}")
    print(f"manifest: {manifest_path}")
    print(f"plans: {len(manifest['plans'])}; ignored 12h limits: {len(manifest['ignored_limits'])}")
    if args.apply:
        if not args.management_key:
            parser.error("--apply requires --management-key or CPA_MANAGEMENT_KEY")
        api_keys = configured_api_keys(raw.get("api-keys"))
        apply_manifest(manifest, ManagementAPI(args.management_url, args.management_key), api_keys)
        print("applied plans and API-key bindings; restart CPA after installing the migrated config")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
