"""A fake ComfyUI for the media step: /prompt queues a workflow and logs it
to prompts.jsonl, /history/<id> says it is done after one look, /view hands
out picture.png, or clip.mp4 for a workflow whose node 12 has a length.

    python3 comfy.py PORT DIR      # DIR holds picture.png and clip.mp4
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

PORT, DIR = int(sys.argv[1]), sys.argv[2]
runs, looks = {}, {}


class Handler(BaseHTTPRequestHandler):
    def send(self, code, body, kind="application/json"):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", kind)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        if self.path != "/prompt":
            return self.send(404, {})
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        pid = "p%d" % (len(runs) + 1)
        runs[pid] = body["prompt"]
        with open(os.path.join(DIR, "prompts.jsonl"), "a") as f:
            f.write(json.dumps(body["prompt"]) + "\n")
        self.send(200, {"prompt_id": pid})

    def do_GET(self):
        u = urlparse(self.path)
        if u.path.startswith("/history/"):
            pid = u.path.split("/")[-1]
            looks[pid] = looks.get(pid, 0) + 1
            if pid not in runs or looks[pid] < 2:
                return self.send(200, {})
            video = "length" in runs[pid].get("12", {}).get("inputs", {})
            out = {"videos" if video else "images": [{"filename": "ComfyUI_%s.%s" % (pid, "mp4" if video else "png"), "subfolder": "", "type": "output"}]}
            return self.send(200, {pid: {"status": {"status_str": "success", "completed": True}, "outputs": {"9": out}}})
        if u.path == "/view":
            name = parse_qs(u.query)["filename"][0]
            with open(os.path.join(DIR, "clip.mp4" if name.endswith(".mp4") else "picture.png"), "rb") as f:
                return self.send(200, f.read(), "application/octet-stream")
        self.send(404, {})

    def log_message(self, *args):
        pass


ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
