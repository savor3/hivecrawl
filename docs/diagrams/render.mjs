// Renders docs/assets/diagrams/*.svg to 2x PNG.
//   npm i puppeteer-core && CHROME=/usr/bin/google-chrome node docs/diagrams/render.mjs
import { readdirSync, readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import puppeteer from "puppeteer-core";

const dir = join(dirname(fileURLToPath(import.meta.url)), "..", "assets", "diagrams");
const browser = await puppeteer.launch({ executablePath: process.env.CHROME || "/usr/bin/google-chrome", headless: "new" });
const page = await browser.newPage();
for (const f of readdirSync(dir).filter((f) => f.endsWith(".svg"))) {
  const svg = readFileSync(join(dir, f), "utf8");
  const [, w, h] = svg.match(/<svg [^>]*width="(\d+)" height="(\d+)"/);
  await page.setViewport({ width: +w, height: +h, deviceScaleFactor: 2 });
  await page.setContent(`<style>html,body{margin:0}</style>${svg}`, { waitUntil: "load" });
  await page.screenshot({ path: join(dir, f.replace(".svg", ".png")), clip: { x: 0, y: 0, width: +w, height: +h } });
  console.log("rendered", f);
}
await browser.close();
