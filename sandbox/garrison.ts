// The platform's tools in Pi. The agent service lists the agent's gateway
// tools (repos, stacks, logs, changes, screenshots, skills) and runs each call
// at the gateway, as the person whose message started the turn. The sandbox
// holds no token of its own: GARRISON_TOKEN only names this chat to the agent
// service, and counts only while a turn runs.
//
// With the gateway's propose_patch, the agent gets propose_workspace_changes
// instead: it turns the changes in a checkout of a platform repo (cloned from
// GARRISON_GIT) into a pull request, by the person or, where only its grants
// allow it, by the agent's own account, and adds later changes of the same
// checkout to it. ask_agent and merge_change come from the agent service and
// the gateway like any other tool.
import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { Type } from "typebox";
import { execFile } from "node:child_process";
import { request } from "node:http";
import { resolve } from "node:path";

const api = process.env.GARRISON_API ?? "";
const token = process.env.GARRISON_TOKEN ?? "";

type Spec = { name: string; description: string; input_schema: Record<string, unknown> };
type Result = { text: string; is_error?: boolean };

// call asks the agent service. node:http rather than fetch: fetch gives up
// after five minutes without an answer, and asking another agent
// (ask_agent) can take longer. The turn's own time limit ends it instead.
function call(path: string, body?: unknown, signal?: AbortSignal): Promise<{ status: number; text: string }> {
  const data = body === undefined ? undefined : JSON.stringify(body);
  return new Promise((ok, fail) => {
    const req = request(
      api + path,
      {
        method: data === undefined ? "GET" : "POST",
        headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
        signal,
      },
      (res) => {
        let text = "";
        res.setEncoding("utf8");
        res.on("data", (c) => (text += c));
        res.on("end", () => ok({ status: res.statusCode ?? 0, text }));
        res.on("error", fail);
      },
    );
    req.on("error", fail);
    req.end(data);
  });
}

async function tool(name: string, args: unknown, signal?: AbortSignal): Promise<string> {
  const r = await call("/tools/call", { name, arguments: args }, signal);
  let out: Result;
  try {
    out = JSON.parse(r.text) as Result;
  } catch {
    out = { text: `HTTP ${r.status}`, is_error: true };
  }
  // Pi marks a tool call as failed when it throws.
  if (r.status !== 200 || out.is_error) throw new Error(out.text);
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
type Proposal = { path: string; title: string; description?: string; base?: string };

async function proposeWorkspace(p: Proposal, signal?: AbortSignal) {
  const dir = resolve("/data/work", p.path || ".");
  const top = (await git(dir, "rev-parse", "--show-toplevel")).trim();
  const origin = await config(top, "remote.origin.url");
  const m = origin.match(/\/git\/([^/]+)\/([^/]+?)(?:\.git)?\/?$/);
  if (!origin.startsWith(api + "/git/") || !m) {
    throw new Error(`${top} is not a clone of a platform repo: clone it from ${api}/git/<namespace>/<repo>.git`);
  }
  const [, namespace, repo] = m;
  const branch = await config(top, "garrison.branch");
  let base = await config(top, "garrison.base");
  if (!branch && p.base) base = p.base;
  if (!base) {
    base = (await git(top, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").catch(() => "origin/main")).trim().replace(/^origin\//, "");
  }
  await git(top, "add", "-A");
  let since: string;
  if (branch) {
    // What was proposed last time is on the branch already.
    since = await config(top, "garrison.proposed");
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
  await git(top, "config", "garrison.branch", onto);
  await git(top, "config", "garrison.base", base);
  await git(top, "config", "garrison.proposed", proposed);
  return text;
}

export default async function (pi: ExtensionAPI) {
  if (!api) return;
  const res = await call("/tools");
  if (res.status !== 200) throw new Error(`garrison: cannot list the tools: ${res.status} ${res.text}`);
  const specs = JSON.parse(res.text) as Spec[];
  for (const spec of specs) {
    if (spec.name === "propose_patch") {
      pi.registerTool({
        name: "propose_workspace_changes",
        label: "propose_workspace_changes",
        description:
          "Proposes the changes in a checkout of a platform repo in your workspace (cloned from $GARRISON_GIT/<namespace>/<repo>.git) as a pull request: by the person you talk to, or by your own account where only your grants allow it. Every file added, changed or deleted since it was cloned goes in. Called again on the same checkout, it adds what changed since to the same pull request. Give the person the link.",
        parameters: Type.Object({
          path: Type.String({ description: "the checkout, relative to /data/work, e.g. todo" }),
          title: Type.String({ description: "short title of the change" }),
          description: Type.Optional(Type.String({ description: "why, and what it does" })),
          base: Type.Optional(Type.String({ description: "the branch the change is for, e.g. dev; default the repo's main branch. Only for the first proposal of a checkout" })),
        }),
        async execute(_id, params, signal) {
          const text = await proposeWorkspace(params as Proposal, signal);
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
