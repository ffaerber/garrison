// The platform's tools in Pi. The agent service lists the agent's gateway
// tools (repos, stacks, logs, changes, screenshots, skills) and runs each call
// at the gateway, as the person whose message started the turn. The sandbox
// holds no token of its own: HOMECLOUD_TOKEN only names this chat to the agent
// service, and counts only while a turn runs.
import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { Type } from "typebox";

const api = process.env.HOMECLOUD_API ?? "";
const token = process.env.HOMECLOUD_TOKEN ?? "";

type Spec = { name: string; description: string; input_schema: Record<string, unknown> };

async function call(path: string, body?: unknown, signal?: AbortSignal): Promise<Response> {
  return fetch(api + path, {
    method: body === undefined ? "GET" : "POST",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  });
}

export default async function (pi: ExtensionAPI) {
  if (!api) return;
  const res = await call("/tools");
  if (!res.ok) throw new Error(`homecloud: cannot list the tools: ${res.status} ${await res.text()}`);
  const specs = (await res.json()) as Spec[];
  for (const spec of specs) {
    pi.registerTool({
      name: spec.name,
      label: spec.name,
      description: spec.description,
      parameters: Type.Unsafe(spec.input_schema),
      async execute(_id, params, signal) {
        const r = await call("/tools/call", { name: spec.name, arguments: params }, signal);
        const out = (await r.json().catch(() => ({ text: `HTTP ${r.status}`, is_error: true }))) as {
          text: string;
          is_error?: boolean;
        };
        // Pi marks a tool call as failed when it throws.
        if (!r.ok || out.is_error) throw new Error(out.text);
        return { content: [{ type: "text", text: out.text }], details: {} };
      },
    });
  }
}
