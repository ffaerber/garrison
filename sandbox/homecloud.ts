// The platform's tools in Pi. The agent service lists the agent's gateway
// tools (repos, stacks, logs, changes, screenshots, skills) and runs each call
// at the gateway, as the person whose message started the turn. The sandbox
// holds no token of its own: HOMECLOUD_TOKEN only names this chat to the agent
// service, and counts only while a turn runs.
//
// With the gateway's propose_patch, the agent gets propose_workspace_changes
// instead: it turns the changes in a checkout of a platform repo (cloned from
// HOMECLOUD_GIT) into a pull request by the person, and adds later changes
// of the same checkout to it.
import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { Type } from "typebox";
import { execFile } from "node:child_process";
import { resolve } from "node:path";

const api = process.env.HOMECLOUD_API ?? "";
const token = process.env.HOMECLOUD_TOKEN ?? "";

type Spec = { name: string; description: string; input_schema: Record<string, unknown> };
type Result = { text: string; is_error?: boolean };

async function call(path: string, body?: unknown, signal?: AbortSignal): Promise<Response> {
  return fetch(api + path, {
    method: body === undefined ? "GET" : "POST",
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  });
}

async function tool(name: string, args: unknown, signal?: AbortSignal): Promise<string> {
  const r = await call("/tools/call", { name, arguments: args }, signal);
  const out = (await r.json().catch(() => ({ text: `HTTP ${r.status}`, is_error: true }))) as Result;
  // Pi marks a tool call as failed when it throws.
  if (!r.ok || out.is_error) throw new Error(out.text);
  return out.text;
}

function git(cwd: string, ...args: string[]): Promise<string> {
  return new Promise((ok, fail) =>
    execFile("git", args, { cwd, maxBuffer: 16 << 20 }, (err, stdout, stderr) =>
      err ? fail(new Error(`git ${args[0]}: ${stderr.trim() || err.message}`)) : ok(stdout),
    ),
  );
}

const config = (cwd: string, key: string) => git(cwd, "config", "--get", key).then((s) => s.trim(), () => "");

// The changes of a checkout as a pull request.
async function proposeWorkspace(p: { path: string; title: string; description?: string }, signal?: AbortSignal) {
  const dir = resolve("/data/work", p.path || ".");
  const top = (await git(dir, "rev-parse", "--show-toplevel")).trim();
  const origin = await config(top, "remote.origin.url");
  const m = origin.match(/\/git\/([^/]+)\/([^/]+?)(?:\.git)?\/?$/);
  if (!origin.startsWith(api + "/git/") || !m) {
    throw new Error(`${top} is not a clone of a platform repo: clone it from ${api}/git/<namespace>/<repo>.git`);
  }
  const [, namespace, repo] = m;
  let base = await config(top, "homecloud.base");
  if (!base) {
    base = (await git(top, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").catch(() => "origin/main")).trim().replace(/^origin\//, "");
  }
  const branch = await config(top, "homecloud.branch");
  await git(top, "add", "-A");
  let since: string;
  if (branch) {
    // What was proposed last time is on the branch already.
    since = await config(top, "homecloud.proposed");
  } else {
    await git(top, "fetch", "-q", "origin", base);
    since = (await git(top, "merge-base", "HEAD", "FETCH_HEAD")).trim();
  }
  const patch = await git(top, "diff", "--cached", "--binary", since);
  if (!patch.trim()) throw new Error(branch ? "nothing changed since the last proposal" : `nothing differs from ${base}`);
  const text = await tool(
    "propose_patch",
    { namespace, repo, base, branch, patch, title: p.title, description: p.description ?? "" },
    signal,
  );
  const proposed = (await git(top, "write-tree")).trim();
  const onto = text.match(/^Branch: (\S+)$/m)?.[1] ?? branch;
  await git(top, "config", "homecloud.branch", onto);
  await git(top, "config", "homecloud.base", base);
  await git(top, "config", "homecloud.proposed", proposed);
  return text;
}

export default async function (pi: ExtensionAPI) {
  if (!api) return;
  const res = await call("/tools");
  if (!res.ok) throw new Error(`homecloud: cannot list the tools: ${res.status} ${await res.text()}`);
  const specs = (await res.json()) as Spec[];
  for (const spec of specs) {
    if (spec.name === "propose_patch") {
      pi.registerTool({
        name: "propose_workspace_changes",
        label: "propose_workspace_changes",
        description:
          "Proposes the changes in a checkout of a platform repo in your workspace (cloned from $HOMECLOUD_GIT/<namespace>/<repo>.git) as a pull request by the person you talk to: every file added, changed or deleted since it was cloned. Called again on the same checkout, it adds what changed since to the same pull request. Nothing changes until a person allowed to merges it in Gitea; give them the link.",
        parameters: Type.Object({
          path: Type.String({ description: "the checkout, relative to /data/work, e.g. todo" }),
          title: Type.String({ description: "short title of the change" }),
          description: Type.Optional(Type.String({ description: "why, and what it does" })),
        }),
        async execute(_id, params, signal) {
          const text = await proposeWorkspace(params as { path: string; title: string; description?: string }, signal);
          return { content: [{ type: "text", text }], details: {} };
        },
      });
      continue;
    }
    pi.registerTool({
      name: spec.name,
      label: spec.name,
      description: spec.description,
      parameters: Type.Unsafe(spec.input_schema),
      async execute(_id, params, signal) {
        return { content: [{ type: "text", text: await tool(spec.name, params, signal) }], details: {} };
      },
    });
  }
}
