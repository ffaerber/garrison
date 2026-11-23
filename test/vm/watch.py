#!/usr/bin/env python3
"""test/vm/watch.py [--port 8099] [--host 127.0.0.1]: a page that follows
test/vm/steps.sh live, at http://localhost:8099.

It shows the tree of steps with each one's state and time, the host's free
memory, the Multipass VMs, and the log of the step picked (or the run's own
output, bin/steps.out), growing as it is written. It only reads what
steps.sh leaves in bin/, so it can start before, during or after a run, and
stopping it changes nothing. Standard library only.
"""

import argparse
import json
import os
import re
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
BIN = os.path.join(ROOT, "bin")
STEPS = os.path.join(ROOT, "test", "vm", "steps.sh")
ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")
STEP_NAME = re.compile(r"^[a-z0-9-]+$")


class Cache:
    """Cache(seconds, fn): fn's result, computed again at most every seconds."""

    def __init__(self, seconds, fn):
        self.seconds, self.fn = seconds, fn
        self.value, self.at, self.lock = None, 0.0, threading.Lock()

    def get(self):
        with self.lock:
            if time.time() - self.at > self.seconds:
                try:
                    self.value = self.fn()
                except Exception as e:  # a slow or failing command shows as an error, not a crash
                    self.value = {"error": str(e)}
                self.at = time.time()
            return self.value


def tree():
    """tree: the steps from `steps.sh --list`, in order, with depth and kind
    and whether each is green; slow (fingerprints), so cached."""
    out = subprocess.run([STEPS, "--list"], cwd=ROOT, capture_output=True, text=True, timeout=120).stdout
    steps = []
    for line in out.splitlines()[1:]:
        m = re.match(r"^( *)(\S+) (\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)", line)
        if m:
            indent, number, name, kind, state, took, vm = m.groups()
            steps.append({"depth": len(indent) // 2, "number": number, "name": name,
                          "kind": kind, "listed": state, "took": took, "vm": vm})
    return steps


def vms():
    """vms: the Multipass VMs and their states."""
    out = subprocess.run(["multipass", "list", "--format", "csv"], capture_output=True, text=True, timeout=30).stdout
    rows = [line.split(",") for line in out.splitlines()[1:] if line]
    return [{"name": r[0], "state": r[1]} for r in rows if len(r) > 1]


TREE = Cache(20, tree)
VMS = Cache(5, vms)


def memory():
    """memory: the host's total and available memory, in GB."""
    info = {}
    with open("/proc/meminfo") as f:
        for line in f:
            k, v = line.split(":")
            info[k] = int(v.split()[0]) / 1048576
    return {"total": round(info["MemTotal"], 1), "available": round(info["MemAvailable"], 1),
            "swap_used": round(info["SwapTotal"] - info["SwapFree"], 1)}


def read(path):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return ""


def running():
    """running: whether the steps.sh whose process ID is in bin/lock-run is
    alive; read, not locked, so a run starting now never finds it taken."""
    pid = read(os.path.join(BIN, "lock-run"))
    return pid.isdigit() and "steps.sh" in read(f"/proc/{pid}/cmdline")


def status():
    """status: the tree with each step's state in the current run
    (bin/state/STEP), its start for one running, and the rest."""
    steps = TREE.get()
    if isinstance(steps, dict):
        steps = []
    now = time.time()
    for s in steps:
        p = os.path.join(BIN, "state", s["name"])
        s["state"] = read(p)
        s["since"] = int(now - os.path.getmtime(p)) if s["state"].startswith("running") else None
        s["has_log"] = os.path.exists(os.path.join(BIN, f"steps-{s['name']}.log"))
    return {"running": running(), "steps": steps, "memory": memory(), "vms": VMS.get(),
            "free_gb": os.environ.get("FREE_GB", "10")}


def log(step, offset):
    """log(step, offset): the step's log (or bin/steps.out for "run") from
    byte offset on, without terminal colours; offset 0 starts at most 200 KB
    from its end."""
    path = os.path.join(BIN, "steps.out" if step == "run" else f"steps-{step}.log")
    try:
        size = os.path.getsize(path)
    except OSError:
        return {"text": "", "offset": 0, "missing": True}
    if offset > size:  # the log began again
        offset = 0
    if offset == 0:
        offset = max(0, size - 200_000)
    with open(path, "rb") as f:
        f.seek(offset)
        data = f.read(1_000_000)
    text = ANSI.sub("", data.decode("utf-8", "replace")).replace("\r", "")
    return {"text": text, "offset": offset + len(data)}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        url = urlparse(self.path)
        q = parse_qs(url.query)
        if url.path == "/":
            self.send(200, "text/html; charset=utf-8", PAGE.encode())
        elif url.path == "/api/status":
            self.json(status())
        elif url.path == "/api/log":
            step = q.get("step", ["run"])[0]
            if step != "run" and not STEP_NAME.match(step):
                return self.send(400, "text/plain", b"bad step")
            self.json(log(step, int(q.get("offset", ["0"])[0] or 0)))
        else:
            self.send(404, "text/plain", b"not found")

    def json(self, value):
        self.send(200, "application/json", json.dumps(value).encode())

    def send(self, code, kind, body):
        self.send_response(code)
        self.send_header("Content-Type", kind)
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):  # quiet: the page polls every two seconds
        pass


PAGE = r"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Garrison tests</title>
<style>
:root { --bg:#fafafa; --panel:#fff; --fg:#1d1d1f; --mute:#6e6e73; --line:#e3e3e6; --green:#1a7f37; --red:#cf222e;
        --blue:#0969da; --amber:#9a6700; --logbg:#0d1117; --logfg:#d1d7e0; }
@media (prefers-color-scheme: dark) { :root { --bg:#0d1117; --panel:#161b22; --fg:#e6edf3; --mute:#8b949e; --line:#30363d;
        --green:#3fb950; --red:#f85149; --blue:#58a6ff; --amber:#d29922; --logbg:#010409; } }
* { box-sizing:border-box; }
body { margin:0; background:var(--bg); color:var(--fg); font:14px/1.4 system-ui, sans-serif; height:100vh; display:flex; flex-direction:column; }
header { display:flex; gap:16px; align-items:center; padding:10px 16px; border-bottom:1px solid var(--line); background:var(--panel); flex-wrap:wrap; }
header h1 { font-size:16px; margin:0; }
.pill { padding:2px 10px; border-radius:99px; font-size:12px; border:1px solid var(--line); }
.pill.on { color:var(--blue); border-color:var(--blue); }
.mem { display:flex; align-items:center; gap:8px; font-size:12px; color:var(--mute); }
.bar { width:160px; height:8px; background:var(--line); border-radius:4px; overflow:hidden; }
.bar i { display:block; height:100%; background:var(--green); }
main { flex:1; display:flex; min-height:0; }
aside { width:360px; overflow:auto; border-right:1px solid var(--line); background:var(--panel); }
.step { display:flex; align-items:center; gap:8px; padding:5px 12px; cursor:pointer; border-left:3px solid transparent; }
.step:hover { background:var(--bg); }
.step.sel { border-left-color:var(--blue); background:var(--bg); }
.dot { width:10px; height:10px; border-radius:50%; background:var(--line); flex:none; }
.green .dot { background:var(--green); } .failed .dot { background:var(--red); } .skipped .dot { background:var(--amber); }
.running .dot { background:var(--blue); animation:pulse 1s infinite alternate; }
@keyframes pulse { to { opacity:.3; } }
.step .n { color:var(--mute); font-variant-numeric:tabular-nums; }
.step .t { margin-left:auto; color:var(--mute); font-size:12px; font-variant-numeric:tabular-nums; }
.step .k { font-size:10px; color:var(--mute); border:1px solid var(--line); border-radius:3px; padding:0 3px; }
h2 { font-size:11px; text-transform:uppercase; letter-spacing:.05em; color:var(--mute); margin:14px 12px 4px; }
.vm { padding:2px 12px; font-size:12px; display:flex; justify-content:space-between; }
.vm .Running { color:var(--blue); }
section { flex:1; display:flex; flex-direction:column; min-width:0; }
.loghead { padding:8px 16px; border-bottom:1px solid var(--line); display:flex; gap:12px; align-items:center; background:var(--panel); }
.loghead label { margin-left:auto; font-size:12px; color:var(--mute); }
pre { flex:1; margin:0; overflow:auto; padding:12px 16px; background:var(--logbg); color:var(--logfg); font:12px/1.45 ui-monospace, monospace; white-space:pre-wrap; word-break:break-all; }
pre .ok { color:#3fb950; } pre .fail { color:#f85149; font-weight:600; } pre .head { color:#58a6ff; } pre .slow { color:#8b949e; }
@media (max-width:700px) { main { flex-direction:column; } aside { width:auto; max-height:40vh; border-right:0; border-bottom:1px solid var(--line); } }
</style></head><body>
<header><h1>Garrison tests</h1><span id="run" class="pill">idle</span>
<div class="mem"><span>memory</span><div class="bar"><i id="membar"></i></div><span id="memtext"></span></div>
<span id="summary" style="color:var(--mute);font-size:12px"></span></header>
<main><aside><div class="step" data-step="run"><span class="dot"></span><b>the run's output</b></div>
<h2>Steps</h2><div id="steps"></div><h2>VMs</h2><div id="vms"></div></aside>
<section><div class="loghead"><b id="logname">the run's output</b><span id="logstate" style="color:var(--mute)"></span>
<label><input type="checkbox" id="follow" checked> follow</label></div><pre id="log"></pre></section></main>
<script>
let step = location.hash.slice(1) || 'run', offset = 0, busy = false;
const $ = id => document.getElementById(id);
const esc = s => s.replace(/[&<>]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]));
const mins = s => s == null ? '' : `${Math.floor(s/60)}m${String(s%60).padStart(2,'0')}s`;
function cls(state) { return (state.match(/^(green|failed|skipped|running)/) || [''])[0]; }
function line(l) {
  const e = esc(l);
  if (/^FAIL/.test(l)) return `<span class="fail">${e}</span>`;
  if (/^ok\b/.test(l)) return `<span class="ok">${e}</span>`;
  if (/^(==>|#)/.test(l)) return `<span class="head">${e}</span>`;
  if (/^slow\b/.test(l)) return `<span class="slow">${e}</span>`;
  return e;
}
function pick(s) { step = s; offset = 0; location.hash = s; $('log').innerHTML = ''; $('logname').textContent = s === 'run' ? "the run's output" : s; mark(); pollLog(); }
function mark() { document.querySelectorAll('.step').forEach(e => e.classList.toggle('sel', e.dataset.step === step)); }
document.addEventListener('click', e => { const s = e.target.closest('.step'); if (s) pick(s.dataset.step); });
async function pollStatus() {
  try {
    const d = await (await fetch('/api/status')).json();
    $('run').textContent = d.running ? 'running' : 'idle'; $('run').classList.toggle('on', d.running);
    const used = d.memory.total - d.memory.available;
    $('membar').style.width = (100 * used / d.memory.total) + '%';
    $('membar').style.background = d.memory.available < d.free_gb ? 'var(--red)' : d.memory.available < 2 * d.free_gb ? 'var(--amber)' : 'var(--green)';
    $('memtext').textContent = `${d.memory.available}G free of ${d.memory.total}G` + (d.memory.swap_used > 0.5 ? `, swap ${d.memory.swap_used}G` : '');
    const count = {}; let cur = '';
    $('steps').innerHTML = d.steps.map(s => {
      const st = s.state || (s.listed === 'green' ? 'green (earlier run)' : s.listed);
      const c = cls(st); count[c || 'other'] = (count[c || 'other'] || 0) + 1;
      if (s.name === step) cur = st;
      const t = s.since != null ? mins(s.since) : (/green|failed/.test(st) && s.took !== '-' ? s.took : '');
      return `<div class="step ${c}" data-step="${s.name}" title="${esc(st)}" style="padding-left:${12 + 16 * s.depth}px">
        <span class="dot"></span><span class="n">${s.number}</span>${s.name}${s.kind === 'fixture' ? ' <span class="k">F</span>' : ''}<span class="t">${t}</span></div>`;
    }).join('');
    $('summary').textContent = ['running','green','failed','skipped'].filter(k => count[k]).map(k => `${count[k]} ${k}`).join(' · ');
    $('logstate').textContent = step === 'run' ? '' : cur;
    $('vms').innerHTML = (Array.isArray(d.vms) ? d.vms : []).map(v => `<div class="vm"><span>${esc(v.name)}</span><span class="${v.state}">${v.state}</span></div>`).join('') || '<div class="vm">none</div>';
    mark();
  } catch (e) { $('run').textContent = 'watch.py is not answering'; }
}
async function pollLog() {
  if (busy) return; busy = true;
  try {
    const want = step, d = await (await fetch(`/api/log?step=${encodeURIComponent(step)}&offset=${offset}`)).json();
    if (want !== step) return;
    const pre = $('log');
    if (d.missing && !pre.textContent) pre.textContent = '(no log yet)';
    if (d.text) {
      if (pre.textContent === '(no log yet)' || d.offset < offset) pre.innerHTML = '';
      pre.insertAdjacentHTML('beforeend', d.text.split('\n').map(line).join('\n'));
      if ($('follow').checked) pre.scrollTop = pre.scrollHeight;
    }
    offset = d.offset;
  } finally { busy = false; }
}
pick(step); pollStatus(); setInterval(pollStatus, 2000); setInterval(pollLog, 1000);
</script></body></html>"""


def main():
    ap = argparse.ArgumentParser(description="Follow test/vm/steps.sh in the browser.")
    ap.add_argument("--port", type=int, default=8099)
    ap.add_argument("--host", default="127.0.0.1", help="0.0.0.0 shows the logs to the whole network")
    a = ap.parse_args()
    print(f"Following test/vm/steps.sh at http://{'localhost' if a.host == '127.0.0.1' else a.host}:{a.port}  (Ctrl-C stops)")
    ThreadingHTTPServer((a.host, a.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
