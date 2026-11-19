#!/usr/bin/env node
// web: a headless browser for agents that reach the internet (the scout).
// It goes through the sandbox's proxy, as far as the agent's internet
// reaches, and carries no sign-in: it sees the web as anyone would.
//
//   web read URL            the page's title, address, text and links
//   web shot URL FILE [WxH] a PNG of the page (default 1440x900), full length
import { chromium } from "playwright-core";

const [cmd, url, file, size] = process.argv.slice(2);
const usage = "usage: web read URL | web shot URL FILE [WIDTHxHEIGHT]";
if (!url || !/^https?:\/\//.test(url) || !["read", "shot"].includes(cmd) || (cmd === "shot" && !file)) {
  console.error(usage);
  process.exit(2);
}
let [width, height] = [1440, 900];
if (size) {
  const m = /^(\d{2,4})x(\d{2,4})$/.exec(size);
  if (!m) { console.error(usage); process.exit(2); }
  [width, height] = [Number(m[1]), Number(m[2])];
}

// The proxy names the sandbox by its user and password, which Chromium
// takes only from here, not from the address.
let proxy;
const raw = process.env.HTTPS_PROXY || process.env.https_proxy;
if (raw) {
  const p = new URL(raw);
  proxy = { server: `${p.protocol}//${p.host}`, username: decodeURIComponent(p.username), password: decodeURIComponent(p.password) };
}

const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM || "/usr/bin/chromium",
  // gVisor is the sandbox; Chromium's own needs what it does not offer.
  // Nothing of its own in the background: only the pages asked for go out.
  args: ["--no-sandbox", "--disable-dev-shm-usage", "--disable-gpu", "--disable-background-networking",
    "--disable-component-update", "--disable-sync", "--no-pings", "--disable-default-apps", "--disable-domain-reliability"],
  proxy,
});
let code = 0;
try {
  const page = await browser.newPage({ viewport: { width, height } });
  const res = await page.goto(url, { waitUntil: "load", timeout: 45000 });
  await page.waitForLoadState("networkidle", { timeout: 5000 }).catch(() => {});
  const status = res ? res.status() : 0;
  if (cmd === "shot") {
    await page.screenshot({ path: file, fullPage: true });
    console.log(`${file}: ${page.url()} (${status}) "${await page.title()}"`);
  } else {
    const text = (await page.evaluate(() => document.body ? document.body.innerText : "")).replace(/\n{3,}/g, "\n\n").slice(0, 40000);
    const links = await page.evaluate(() => [...document.querySelectorAll("a[href]")]
      .map(a => [a.innerText.trim().replace(/\s+/g, " ").slice(0, 80), a.href])
      .filter(([t, h]) => t && /^https?:/.test(h)).slice(0, 80));
    console.log(`Title: ${await page.title()}\nURL: ${page.url()} (${status})\n\n${text}\n\nLinks:`);
    for (const [t, h] of links) console.log(`- ${t}: ${h}`);
  }
  if (status >= 400) code = 1;
} catch (e) {
  console.error(`web: ${e.message.split("\n")[0]}`);
  code = 1;
} finally {
  await browser.close();
}
process.exit(code);
