"use strict";

const $ = (sel) => document.querySelector(sel);

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v; else e.setAttribute(k, v);
  }
  for (const c of children) if (c != null) e.append(c instanceof Node ? c : String(c));
  return e;
}

async function api(path, body) {
  const opts = body === undefined ? {} : {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
  };
  const res = await fetch(path, opts);
  if (res.status === 401) { location.href = "/login"; throw new Error("not signed in"); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

// ---------- who ----------
let me = null;
async function loadMe() {
  me = await api("/api/me");
  $("#me").replaceChildren(me.name, el("span", { class: "role" }, me.admin ? "root admin" : "user"));
}

// ---------- stacks ----------
async function loadStacks() {
  try {
    const stacks = await api("/api/stacks");
    const rows = stacks.map((s) => el("tr", {},
      el("td", {}, s.name),
      el("td", {}, s.commit ? el("code", { title: s.commit }, s.commit.slice(0, 10)) : el("span", { class: "hint" }, "app")),
      el("td", {}, ...s.services.map((svc) => el("span", { class: "svc", title: svc.image },
        el("span", { class: "dot" + (svc.healthy ? "" : " bad") }), svc.name.slice(s.name.length + 1) + " " + svc.replicas)))));
    $("#stacks tbody").replaceChildren(...rows);
    $("#stacks-updated").textContent = "updated " + new Date().toLocaleTimeString();
  } catch (e) {
    $("#stacks-updated").textContent = e.message;
  }
}

// ---------- changes ----------
function diffView(text) {
  const pre = el("pre");
  for (const line of (text || "").split("\n")) {
    const cls = line.startsWith("+") && !line.startsWith("+++") ? "add" : line.startsWith("-") && !line.startsWith("---") ? "del" : "";
    pre.append(el("span", cls ? { class: cls } : {}, line + "\n"));
  }
  return pre;
}

async function loadChanges() {
  try {
    const changes = await api("/api/changes");
    $("#changes-count").textContent = changes.length ? String(changes.length) : "";
    if (!changes.length) { $("#changes").replaceChildren(el("p", { class: "hint" }, "Nothing is waiting.")); return; }
    $("#changes").replaceChildren(...changes.map(changeCard));
  } catch (e) {
    $("#changes").replaceChildren(el("p", { class: "hint" }, e.message));
  }
}

function changeCard(c) {
  const note = el("span", { class: "note" });
  const approve = el("button", { type: "button" }, "Approve");
  const reject = el("button", { type: "button", class: "danger" }, "Reject");
  const decide = async (action, button) => {
    approve.disabled = reject.disabled = true;
    try { await api(`/api/changes/${encodeURIComponent(c.repo)}/${c.number}/${action}`, {}); await loadChanges(); }
    catch (e) { note.textContent = e.message; approve.disabled = reject.disabled = false; }
  };
  approve.addEventListener("click", () => decide("approve", approve));
  reject.addEventListener("click", () => decide("reject", reject));
  const details = el("details", {}, el("summary", {}, "Diff"), diffView(c.diff));
  return el("div", { class: "change" },
    el("h3", {}, c.title),
    el("div", { class: "meta" }, `${c.repo}#${c.number} by ${c.author} · `, el("a", { href: c.url, target: "_blank", rel: "noopener" }, "in Gitea")),
    c.body ? el("p", {}, c.body) : null,
    details,
    el("div", { class: "actions" }, approve, reject, note));
}

// ---------- chat ----------
function addMessage(who, text, cls, steps) {
  const m = el("div", { class: "msg " + cls }, el("div", { class: "who" }, who), text);
  if (steps && steps.length) {
    const d = el("details", { class: "steps" }, el("summary", {}, `${steps.length} step${steps.length > 1 ? "s" : ""}`));
    for (const s of steps) {
      d.append(el("div", { class: "step" + (s.error ? " failed" : "") }, el("code", {}, s.tool), " ", s.args || "", el("pre", {}, s.result || "")));
    }
    m.append(d);
  }
  $("#log").append(m);
  m.scrollIntoView({ block: "end" });
  return m;
}

async function ask(ev) {
  ev.preventDefault();
  const box = $("#message"), button = $("#ask button");
  const text = box.value.trim();
  if (!text) return;
  $("#log .hint")?.remove();
  addMessage(me ? me.name : "you", text, "you");
  box.value = "";
  button.disabled = true;
  const waiting = addMessage("agent", "Thinking…", "agent");
  try {
    const r = await api("/api/chat", { message: text });
    waiting.remove();
    addMessage("agent", r.reply || "(no answer)", "agent", r.steps);
    if ((r.steps || []).some((s) => s.tool === "propose_change" && !s.error)) loadChanges();
  } catch (e) {
    waiting.remove();
    addMessage("agent", e.message, "agent error");
  } finally {
    button.disabled = false;
    box.focus();
  }
}

$("#ask").addEventListener("submit", ask);
$("#message").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); $("#ask").requestSubmit(); }
});
$("#reset").addEventListener("click", async () => {
  await api("/api/chat/reset", {});
  $("#log").replaceChildren(el("p", { class: "hint" }, "New conversation."));
});

loadMe().catch(() => {});
loadStacks();
loadChanges();
setInterval(loadStacks, 10000);
setInterval(loadChanges, 30000);
