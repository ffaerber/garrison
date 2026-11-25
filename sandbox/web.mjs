#!/usr/bin/env node
// web: a headless browser for agents that reach the internet (the scout).
// It goes through the sandbox's proxy, as far as the agent's internet
// reaches, and carries no sign-in: it sees the web as anyone would.
//
//   web read URL               the page's title, address, text and links
//   web shot URL FILE [WxH]    a PNG of the page (default 1440x900), full length
//   web map URL                the site's pages: its sitemap, else the page's links on the site
//   web crawl URL [MAX] [DIR]  reads up to MAX pages of the site (20, at most 100), from URL
//                              on, each into DIR (/data/work/crawl-<host>) as a text file
import { chromium } from "playwright-core";
import { mkdirSync, writeFileSync } from "node:fs";

const [cmd, url, arg3, arg4] = process.argv.slice(2);
const usage = "usage: web read URL | web shot URL FILE [WIDTHxHEIGHT] | web map URL | web crawl URL [MAX] [DIR]";
if (!url || !/^https?:\/\//.test(url) || !["read", "shot", "map", "crawl"].includes(cmd) || (cmd === "shot" && !arg3)) {
  console.error(usage);
  process.exit(2);
}
const file = cmd === "shot" ? arg3 : undefined, size = cmd === "shot" ? arg4 : undefined;
let [width, height] = [1440, 900];
if (size) {
  const m = /^(\d{2,4})x(\d{2,4})$/.exec(size);
  if (!m) { console.error(usage); process.exit(2); }
  [width, height] = [Number(m[1]), Number(m[2])];
}
let max = 20;
if (cmd === "crawl" && arg3) {
  max = Number(arg3);
  if (!Number.isInteger(max) || max < 1) { console.error(usage); process.exit(2); }
  max = Math.min(max, 100);
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
// open loads u in a new page and waits for it to settle; it answers the page
// and its HTTP status.
const context = await browser.newContext({ viewport: { width, height } });
async function open(u) {
  const page = await context.newPage();
  const res = await page.goto(u, { waitUntil: "load", timeout: 45000 });
  await page.waitForLoadState("networkidle", { timeout: 5000 }).catch(() => {});
  return [page, res ? res.status() : 0];
}
// text is a page's visible text, links the links on it with their words.
const text = (page, n) => page.evaluate(() => document.body ? document.body.innerText : "").then(t => t.replace(/\n{3,}/g, "\n\n").slice(0, n));
const links = (page) => page.evaluate(() => [...document.querySelectorAll("a[href]")]
  .map(a => [a.innerText.trim().replace(/\s+/g, " ").slice(0, 80), a.href.split("#")[0]])
  .filter(([, h]) => /^https?:/.test(h)));
// onSite: an address on the same site as the start, a page rather than a file.
const start = new URL(url);
const onSite = (h) => { try { const u = new URL(h); return u.host === start.host && !/\.(pdf|zip|png|jpe?g|gif|svg|webp|mp4|mp3|css|js|xml|ico)$/i.test(u.pathname); } catch { return false; } };

// sitemap: the addresses of the site's sitemap (robots.txt names it, or
// /sitemap.xml), following one level of sitemap indexes; [] without one.
async function sitemap() {
  // Through the browser, which goes through the sandbox's proxy.
  const get = async (u) => { try { const r = await context.request.get(u, { timeout: 20000 }); return r.ok() ? await r.text() : ""; } catch { return ""; } };
  const robots = await get(`${start.origin}/robots.txt`);
  const maps = [...robots.matchAll(/^sitemap:\s*(\S+)/gim)].map(m => m[1]);
  if (!maps.length) maps.push(`${start.origin}/sitemap.xml`);
  const out = new Set();
  for (const m of maps.slice(0, 5)) {
    const xml = await get(m);
    const locs = [...xml.matchAll(/<loc>\s*([^<\s]+)\s*<\/loc>/g)].map(x => x[1].replace(/&amp;/g, "&"));
    if (/<sitemapindex/i.test(xml)) {
      for (const sub of locs.slice(0, 10)) {
        for (const x of (await get(sub)).matchAll(/<loc>\s*([^<\s]+)\s*<\/loc>/g)) out.add(x[1].replace(/&amp;/g, "&"));
      }
    } else locs.forEach(l => out.add(l));
    if (out.size >= 2000) break;
  }
  return [...out].filter(onSite);
}

let code = 0;
try {
  if (cmd === "map") {
    let pages = await sitemap();
    let from = "its sitemap";
    if (!pages.length) {
      const [page] = await open(url);
      pages = [...new Set((await links(page)).map(([, h]) => h).filter(onSite))];
      from = "the links on " + page.url();
    }
    console.log(`${pages.length} pages of ${start.host}, from ${from}:`);
    for (const p of pages.slice(0, 500)) console.log(p);
    if (pages.length > 500) console.log(`... and ${pages.length - 500} more`);
  } else if (cmd === "crawl") {
    const dir = arg4 || `/data/work/crawl-${start.host}`;
    mkdirSync(dir, { recursive: true });
    const queue = [url.split("#")[0]], seen = new Set(queue);
    let n = 0;
    while (queue.length && n < max) {
      const u = queue.shift();
      let page;
      try {
        let status;
        [page, status] = await open(u);
        n++;
        const name = `${dir}/${String(n).padStart(3, "0")}.txt`;
        writeFileSync(name, `Title: ${await page.title()}\nURL: ${page.url()} (${status})\n\n${await text(page, 100000)}\n`);
        console.log(`${name}: ${page.url()} (${status}) "${(await page.title()).slice(0, 80)}"`);
        for (const [, h] of await links(page)) {
          if (onSite(h) && !seen.has(h)) { seen.add(h); queue.push(h); }
        }
      } catch (e) {
        console.log(`skipped ${u}: ${e.message.split("\n")[0]}`);
      } finally {
        if (page) await page.close();
      }
    }
    console.log(`Read ${n} pages into ${dir}; ${queue.length} more found on the site. Search them with rg.`);
  } else {
    const [page, status] = await open(url);
    if (cmd === "shot") {
      await page.screenshot({ path: file, fullPage: true });
      console.log(`${file}: ${page.url()} (${status}) "${await page.title()}"`);
    } else {
      console.log(`Title: ${await page.title()}\nURL: ${page.url()} (${status})\n\n${await text(page, 40000)}\n\nLinks:`);
      for (const [t, h] of (await links(page)).filter(([t]) => t).slice(0, 80)) console.log(`- ${t}: ${h}`);
    }
    if (status >= 400) code = 1;
  }
} catch (e) {
  console.error(`web: ${e.message.split("\n")[0]}`);
  code = 1;
} finally {
  await browser.close();
}
process.exit(code);
