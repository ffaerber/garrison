#!/usr/bin/env python3
"""Makes the walkthrough's pictures from the browser test's screenshots.

The UI step of the VM test (test/vm/steps.sh ui, test/ui/scenarios.py)
clicks through Gitea, Open WebUI and the dashboard as alice and bob, and
saves a screenshot of each step in bin/ui-shots/, named NNN-<person>-<what>.png.
This picks the ones the walkthrough shows, makes them smaller, and strings
some into GIFs, into docs/walkthrough/img/:

    python3 docs/walkthrough/make.py [SHOTS]     # SHOTS: bin/ui-shots

Needs Pillow. A screenshot it cannot find is named, and nothing is written.
"""

import os
import re
import sys

from PIL import Image

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "img")
WIDTH = 1000  # of every picture; the screenshots are 1400 wide

# A shot: its name without number and .png, and which of the shots of that
# name (0: the first, -1: the last), as the same step can repeat.
STILLS = {
    # 02 signing in
    "sign-in-keycloak": ("alice-keycloak", 0),
    "sign-in-dashboard": ("alice-dashboard", 0),
    "sign-in-chat": ("alice-chat", 0),
    # 03 a namespace
    "namespace-form": ("alice-new-organisation", 0),
    "namespace-created": ("alice-created-shop", 0),
    "namespace-welcome": ("alice-welcome-in-the-channel", 0),
    "namespace-feed": ("alice-feed-shop-is-set-up", 0),
    "secrets": ("alice-secrets", 0),
    # 04 an app and its stack
    "app-ask": ("alice-asked-operator", 0),
    "app-answer": ("alice-answer-of-garrison-operator", 0),
    "app-card": ("alice-card-of-the-stack", 0),
    "app-merged": ("alice-merged-shop-config-1", 0),
    "app-live": ("alice-web-on-main", 0),
    # 05 dev
    "dev-branch": ("alice-branch-dev", 0),
    "dev-up": ("alice-dev-web-is-up", 0),
    "dev-ask": ("alice-asked-operator", 1),
    "dev-merged": ("alice-merged-shop-web-1", 0),
    "dev-to-main": ("alice-opened-green-background", 0),
    "dev-main-merged": ("alice-merged-shop-web-2", 0),
    "dev-green": ("alice-web-is-green", 0),
    # 06 the agents at work
    "agents-ask-screenshot": ("alice-asked-operator", 2),
    "agents-screenshot": ("alice-screenshot-in-gitea", 0),
    "agents-commander": ("bob-asked-the-commander", 0),
    # 07 a second person
    "members-ask": ("alice-asked-operator", 3),
    "members-approved": ("admin-approved-shop-config-2", 0),
    "members-refused": ("bob-edited-index-html", 0),
    "members-proposed": ("bob-opened-bob-s-heading", 0),
    "members-approve": ("alice-approved-shop-web-4", 0),
    "members-merged": ("bob-merged-shop-web-4", 0),
    # 08 routines
    "routines-edit": ("alice-edited-routines-yaml", 0),
    "routines-proposed": ("alice-opened-two-routines", 0),
    "routines-greeting": ("alice-greeting-in-the-channel", 0),
    "routines-waiting": ("bob-waiting-for-you", 0),
    "routines-reply": ("bob-replied", 0),
    "routines-went-on": ("bob-the-routine-went-on", 0),
    # 09 scheduled jobs
    "jobs-edit": ("alice-edited-gitea-workflows-nightly-yaml", 0),
    "jobs-proposed": ("alice-opened-a-nightly-report", 0),
    "jobs-run": ("alice-scheduled-run", 0),
    "jobs-disabled": ("alice-disabled-nightly-yaml", 0),
}

# Clips: stills in order, each shown for SECONDS.
SECONDS = 2.5
CLIPS = {
    "sign-in": ["sign-in-keycloak", "sign-in-chat", "sign-in-dashboard"],
    "namespace": ["namespace-form", "namespace-created", "namespace-welcome", "namespace-feed"],
    "app": ["app-ask", "app-answer", "app-card", "app-merged", "app-live"],
    "dev": ["dev-branch", "dev-up", "dev-ask", "dev-merged", "dev-to-main", "dev-main-merged", "dev-green"],
    "members": ["members-ask", "members-approved", "members-refused", "members-proposed", "members-approve", "members-merged"],
    "routines": ["routines-edit", "routines-proposed", "routines-greeting", "routines-waiting", "routines-reply", "routines-went-on"],
    "jobs": ["jobs-edit", "jobs-proposed", "jobs-run", "jobs-disabled"],
}


def shots(folder: str) -> dict:
    """{name: [paths, in order]} of NNN-<name>.png."""
    out = {}
    for f in sorted(os.listdir(folder)):
        m = re.fullmatch(r"\d{3}-(.+)\.png", f)
        if m:
            out.setdefault(m.group(1), []).append(os.path.join(folder, f))
    return out


def smaller(path: str) -> Image.Image:
    img = Image.open(path).convert("RGB")
    return img.resize((WIDTH, round(img.height * WIDTH / img.width)), Image.LANCZOS)


def main():
    folder = sys.argv[1] if len(sys.argv) > 1 else "bin/ui-shots"
    have = shots(folder)
    found, missing = {}, []
    for still, (name, which) in STILLS.items():
        try:
            found[still] = have[name][which]
        except (KeyError, IndexError):
            missing.append(f"{still}: no {name} #{which} in {folder}")
    if missing:
        sys.exit("missing screenshots:\n  " + "\n  ".join(missing))
    os.makedirs(OUT, exist_ok=True)
    for f in os.listdir(OUT):
        if f.endswith((".png", ".gif")):
            os.remove(os.path.join(OUT, f))
    for still, path in found.items():
        # 256 colours: the pages are flat, and a third the size.
        smaller(path).quantize(256, method=Image.Quantize.MEDIANCUT).save(os.path.join(OUT, still + ".png"), optimize=True)
    for clip, stills in CLIPS.items():
        frames = [smaller(found[s]) for s in stills]
        height = max(f.height for f in frames)
        canvas = []
        for f in frames:
            c = Image.new("RGB", (WIDTH, height), "white")
            c.paste(f, (0, 0))
            canvas.append(c.quantize(128, method=Image.Quantize.MEDIANCUT))
        canvas[0].save(os.path.join(OUT, clip + ".gif"), save_all=True, append_images=canvas[1:],
                       duration=int(SECONDS * 1000), loop=0, optimize=True)
    total = sum(os.path.getsize(os.path.join(OUT, f)) for f in os.listdir(OUT))
    print(f"{len(found)} pictures and {len(CLIPS)} clips in {OUT}, {total // 1024} KiB")


if __name__ == "__main__":
    main()
