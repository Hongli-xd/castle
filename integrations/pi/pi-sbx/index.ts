/**
 * pi-sbx — route pi's bash tool into a sandboxd sandbox.
 *
 * Deployment model (pi docs: "Route tools through Gondolin" pattern):
 * the pi process and provider credentials stay on the host; bash commands
 * execute inside an isolated sandboxd sandbox through the REST API.
 *
 * Environment:
 *   SBX_URL         daemon base URL      (default http://127.0.0.1:8080)
 *   SBX_API_KEY     tenant API key       (required)
 *   SBX_TEMPLATE    template id to boot  (required unless SBX_SANDBOX_ID)
 *   SBX_SANDBOX_ID  attach to an existing sandbox instead of creating one
 *   SBX_CPU / SBX_MEM / SBX_TTL  spec overrides (millicores / MiB / seconds)
 *   SBX_WORKDIR     host dir mounted read-only into the sandbox (default: process.cwd())
 *   SBX_MOUNT_TARGET in-sandbox path the workdir appears at (default /workspace)
 *   SBX_NO_MOUNT    "1" disables the workdir mount entirely (old behaviour)
 *   SBX_KEEP        "1" keeps the sandbox alive on session shutdown
 *
 * The workdir mount makes the project visible to bash inside the sandbox,
 * read-only: `cat`, `ls`, `python test.py` all work against the real files,
 * but nothing the sandbox writes lands back on the host. The daemon must
 * allowlist the host directory (config `mount.allow`) or the create is
 * refused — an honest refusal, not a silent "your files just aren't there".
 *
 * Install:
 *   mkdir -p ~/.pi/agent/extensions && cp -R pi-sbx ~/.pi/agent/extensions/
 *   pi -e ~/.pi/agent/extensions/pi-sbx
 */

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import {
	createBashTool,
	type BashOperations,
} from "@earendil-works/pi-coding-agent";

const SBX_URL = (process.env.SBX_URL ?? "http://127.0.0.1:8080").replace(/\/$/, "");
const SBX_API_KEY = process.env.SBX_API_KEY ?? "";
const SBX_TEMPLATE = process.env.SBX_TEMPLATE ?? "";
const SBX_SANDBOX_ID = process.env.SBX_SANDBOX_ID ?? "";
const SBX_KEEP = process.env.SBX_KEEP === "1";
const SBX_WORKDIR = process.env.SBX_WORKDIR ?? process.cwd();
const SBX_MOUNT_TARGET = process.env.SBX_MOUNT_TARGET ?? "/workspace";
const SBX_NO_MOUNT = process.env.SBX_NO_MOUNT === "1";
// The workdir mount is what makes the project visible inside the sandbox.
// When it is off, bash runs from "/" with no workdir passthrough (old path).
const mountEnabled = !SBX_NO_MOUNT && SBX_WORKDIR !== "";
const bashCwd = mountEnabled ? SBX_MOUNT_TARGET : "/";
const DEFAULT_EXEC_TIMEOUT = 120; // seconds

class SandboxdError extends Error {
	status: number;
	code: string;

	constructor(status: number, code: string, message: string) {
		super(`sandboxd [${code}] (${status}): ${message}`);
		this.status = status;
		this.code = code;
	}
}

/** Minimal REST client: one sandbox, bash-exec semantics. */
class SandboxClient {
	// plain fields, no parameter properties: Node runs extensions in
	// strip-only TypeScript mode which rejects them
	sandboxId: string;

	constructor(sandboxId: string) {
		this.sandboxId = sandboxId;
	}

	get id(): string {
		return this.sandboxId;
	}

	private async request(method: string, path: string, body?: unknown): Promise<any> {
		const resp = await fetch(`${SBX_URL}${path}`, {
			method,
			headers: {
				Authorization: `Bearer ${SBX_API_KEY}`,
				...(body !== undefined ? { "Content-Type": "application/json" } : {}),
			},
			body: body !== undefined ? JSON.stringify(body) : undefined,
		});
		const text = await resp.text();
		let payload: any = undefined;
		try {
			payload = text ? JSON.parse(text) : undefined;
		} catch {
			payload = { raw: text };
		}
		if (!resp.ok) {
			const err = payload?.error ?? {};
			throw new SandboxdError(resp.status, err.code ?? "unknown", err.message ?? text);
		}
		return payload;
	}

	static async acquire(): Promise<SandboxClient> {
		if (!SBX_API_KEY) {
			throw new Error("SBX_API_KEY is required");
		}
		if (SBX_SANDBOX_ID) {
			const client = new SandboxClient(SBX_SANDBOX_ID);
			await client.request("GET", `/v1/sandboxes/${SBX_SANDBOX_ID}`);
			return client;
		}
		if (!SBX_TEMPLATE) {
			throw new Error("SBX_TEMPLATE or SBX_SANDBOX_ID is required");
		}
		const spec: Record<string, unknown> = {};
		if (process.env.SBX_CPU) spec.cpu_millicores = Number(process.env.SBX_CPU);
		if (process.env.SBX_MEM) spec.memory_mib = Number(process.env.SBX_MEM);
		if (process.env.SBX_TTL) spec.timeout_seconds = Number(process.env.SBX_TTL);
		if (mountEnabled) {
			spec.mounts = [{ source: SBX_WORKDIR, target: SBX_MOUNT_TARGET }];
		}
		const created = await new SandboxClient("").request("POST", "/v1/sandboxes?wait=true", {
			template_id: SBX_TEMPLATE,
			...(Object.keys(spec).length ? { spec } : {}),
		});
		return new SandboxClient(created.id);
	}

	async exec(command: string, timeoutSeconds: number, workdir?: string): Promise<{ exitCode: number; stdout: string; stderr: string }> {
		const result = await this.request("POST", `/v1/sandboxes/${this.sandboxId}/exec`, {
			command,
			timeout_ms: Math.max(1, Math.round(timeoutSeconds * 1000)),
			max_output: 1 << 20,
			...(workdir ? { workdir } : {}),
		});
		const decode = (b64: string | null | undefined): string => {
			if (!b64) return "";
			return Buffer.from(b64, "base64").toString("utf8");
		};
		return {
			exitCode: result.exit_code ?? 0,
			stdout: decode(result.stdout),
			stderr: decode(result.stderr),
		};
	}

	async release(): Promise<void> {
		if (SBX_KEEP) return; // leave the sandbox running (e.g. to inspect later)
		try {
			await this.request("DELETE", `/v1/sandboxes/${this.sandboxId}?force=true`);
		} catch {
			// shutdown path: daemon may already be gone; nothing to do
		}
	}
}

export default function (pi: ExtensionAPI) {
	let client: SandboxClient | undefined;
	let starting: Promise<SandboxClient> | undefined;

	async function acquire(): Promise<SandboxClient> {
		if (client) return client;
		if (!starting) {
			starting = SandboxClient.acquire()
				.then((c) => {
					client = c;
					return c;
				})
				.finally(() => {
					starting = undefined;
				});
		}
		return starting;
	}

	function bashOperations(): BashOperations {
		return {
			exec: async (command, _cwd, { onData, signal, timeout }) => {
				const active = await acquire();
				const effectiveTimeout = timeout && timeout > 0 ? timeout : DEFAULT_EXEC_TIMEOUT;
				const controller = new AbortController();
				const onAbort = () => controller.abort();
				signal?.addEventListener("abort", onAbort, { once: true });
				const timer =
					effectiveTimeout > 0
						? setTimeout(() => controller.abort(), effectiveTimeout * 1000)
						: undefined;
				try {
					// The daemon enforces the hard deadline server-side; the
					// client abort just stops waiting for the response. The
					// workdir passthrough (mount enabled) makes relative paths
					// resolve against the mounted project, so `python test.py`
					// runs where the agent thinks it is.
					const result = await active.exec(command, effectiveTimeout + 5, mountEnabled ? _cwd : undefined);
					// BashOperations streams output through onData as bytes —
					// dropping it here would leave the agent blind to results.
					const encoder = new TextEncoder();
					if (result.stdout) onData(encoder.encode(result.stdout));
					if (result.stderr) onData(encoder.encode(result.stderr));
					return { exitCode: result.exitCode };
				} catch (error) {
					if (signal?.aborted) throw new Error("aborted");
					throw error;
				} finally {
					if (timer) clearTimeout(timer);
					signal?.removeEventListener("abort", onAbort);
				}
			},
		};
	}

	pi.registerTool({
		...createBashTool(bashCwd, { operations: bashOperations() }),
		description: mountEnabled
			? `Runs a bash command inside the sandboxd sandbox (isolated filesystem, no host write access). ` +
			  `The host working directory (${SBX_WORKDIR}) is mounted read-only at ${SBX_MOUNT_TARGET}, so ` +
			  `cat/ls/python all see the real project, but nothing written inside the sandbox reaches the host.`
			: "Runs a bash command inside the sandboxd sandbox (isolated filesystem, no host access). " +
			  "The host working directory is NOT visible; set SBX_WORKDIR (and the daemon's mount.allow) to mount it read-only.",
	});

	pi.registerCommand("sbx", {
		description: "Show sandboxd sandbox status",
		handler: async (_args, ctx) => {
			const active = await acquire();
			ctx.ui.notify(`sandboxd sandbox: ${active.id} @ ${SBX_URL}${SBX_KEEP ? " (kept after session)" : ""}`, "info");
		},
	});

	pi.on("session_shutdown", async () => {
		const active = client;
		client = undefined;
		if (active) await active.release();
	});
}
