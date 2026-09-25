#!/usr/bin/env node
// Harness that loads the REAL pi-sbx extension (Node type-stripping), stubs
// the pi ExtensionAPI surface, and drives the bash-tool path end-to-end
// against a live sandboxd daemon. No LLM key needed: this tests the
// extension, not the model.
import { spawn } from "node:child_process";
import { execSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.join(here, "..");
const extDir = path.join(root, "integrations", "pi", "pi-sbx");
const BASE = "http://127.0.0.1:18084";
const ADMIN = "sk_harness-fixed-key-for-tests-0000000000";

const dataDir = mkdtempSync(path.join(os.tmpdir(), "pi-sbx-"));
const confPath = path.join(dataDir, "sbxd.conf");
writeFileSync(confPath, [
	'listen = "127.0.0.1:18084"',
	`data_dir = "${path.join(dataDir, "data")}"`,
	'bootstrap.image = "sandboxd-test-base:alpine322"',
	"pool.enabled = false",
	`bootstrap_admin_key = "${ADMIN}"`,
	"",
].join("\n"));

try { execSync(`pkill -9 -f 'sandboxd -config ${confPath}' || true`); } catch {}
const daemon = spawn(path.join(root, "bin", "sandboxd"), ["-config", confPath], { stdio: "ignore" });
await new Promise((r) => setTimeout(r, 2000));

let pass = 0, fail = 0;
const ok = (m) => { pass++; console.log("  ✓ " + m); };
const bad = (m) => { fail++; console.log("  ✗ " + m); };

try {
	const api = async (method, p, body, key = ADMIN) => {
		const r = await fetch(BASE + p, {
			method,
			headers: { Authorization: `Bearer ${key}`, ...(body ? { "Content-Type": "application/json" } : {}) },
			body: body ? JSON.stringify(body) : undefined,
		});
		const text = await r.text();
		let data = {};
		try { data = text ? JSON.parse(text) : {}; } catch {}
		return { status: r.status, data };
	};

	const tenant = (await api("POST", "/v1/tenants", { name: "pi-harness" })).data.id;
	const { data: keyData } = await api("POST", `/v1/tenants/${tenant}/keys`, { name: "harness" });
	const TKEY = keyData.secret;
	const tpl = (await api("GET", "/v1/templates", undefined, TKEY)).data.templates?.[0]?.id;
	if (!tpl) throw new Error("no template seeded");

	process.env.SBX_URL = BASE;
	process.env.SBX_API_KEY = TKEY;
	process.env.SBX_TEMPLATE = tpl;
	process.env.SBX_CPU = "500";
	process.env.SBX_MEM = "256";
	const mod = await import(path.join(extDir, "index.ts"));

	const registered = { tools: {}, commands: {}, handlers: {} };
	mod.default({
		registerTool: (t) => { registered.tools[t.name ?? "bash"] = t; },
		registerCommand: (name, c) => { registered.commands[name] = c; },
		on: (event, h) => { registered.handlers[event] = h; },
	});

	if (registered.tools.bash) ok("bash tool override registered"); else bad("bash tool not registered");
	if (registered.handlers.session_shutdown) ok("shutdown handler registered"); else bad("no shutdown handler");

	// stub the slices of pi's ExtensionContext the bash tool touches
	const ui = {
		ui: { setStatus() {}, notify() {}, theme: { fg: () => "" } },
		sessionManager: { getSessionId: () => "harness-session", getSessionFile: () => "/tmp/harness-session.jsonl" },
		model: "harness-model",
	};
	// pi's bash tool packages ops output into result.content[].text and
	// throws a typed error carrying the output for non-zero exits
	const resultText = (result) =>
		(result?.content ?? []).map((c) => c.text ?? "").join("");
	const result = await registered.tools.bash.execute(
		"call-1",
		{ command: "echo pi-sbx-works && uname -m" },
		undefined,
		() => {},
		ui,
	);
	const streamed = resultText(result);
	if (streamed.includes("pi-sbx-works")) ok("command output surfaced in tool result"); else bad("no output: " + JSON.stringify(result));
	if (streamed.includes("aarch64") || streamed.includes("x86_64")) ok("real execution inside the sandbox (arch visible)"); else bad("arch missing");

	let isoText = "";
	try {
		const iso = await registered.tools.bash.execute("call-2", { command: "test -d /workspace && echo mounted || echo not-mounted" }, undefined, () => {}, ui);
		isoText = resultText(iso);
	} catch (e) { isoText = String(e?.message); }
	if (isoText.includes("not-mounted")) ok("host workspace NOT mounted (harder boundary by design)"); else bad("workspace state: " + isoText);

	let exitMsg = "";
	try {
		await registered.tools.bash.execute("call-3", { command: "exit 7" }, undefined, () => {}, ui);
	} catch (e) { exitMsg = String(e?.message ?? e); }
	if (/7/.test(exitMsg)) ok("non-zero exit surfaced (" + exitMsg.trim().split("\n").pop() + ")"); else bad("exit 7 not surfaced: " + exitMsg);

	const before = (await api("GET", "/v1/sandboxes", undefined, TKEY)).data.count;
	await registered.handlers.session_shutdown(undefined, ui);
	await new Promise((r) => setTimeout(r, 800));
	const after = (await api("GET", "/v1/sandboxes", undefined, TKEY)).data.count;
	if (before >= 1 && after === 0) ok(`session_shutdown released the sandbox (${before} → 0)`); else bad(`sandbox not released (${before} → ${after})`);
} catch (e) {
	bad("harness error: " + (e && e.message));
}

console.log(`\npi-sbx harness results: pass=${pass} fail=${fail}`);
daemon.kill("SIGKILL");
rmSync(dataDir, { recursive: true, force: true });
process.exit(fail === 0 ? 0 : 1);
