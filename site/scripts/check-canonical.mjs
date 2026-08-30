// Assert that every page's canonical URL is the address the rest of the site
// actually publishes.
//
// Starlight derives the canonical from the file it wrote to disk, and under
// `build.format: "file"` that carries a `.html` nothing else here uses: the
// sitemap lists `/start/install`, links point at `/start/install`, `llms.txt`
// points at `/start/install.md`, and Cloudflare Pages 301s the `.html` form to
// the extensionless one. `src/starlight-route-data.ts` corrects the tags, and
// this asserts the correction still holds — the failure it guards against is
// silent, produces a build that looks perfect, and is only visible in a crawler
// weeks later.
//
// Three properties, because each fails on its own:
//   - the canonical carries no `.html`, so it is not naming a redirect;
//   - `og:url` says the same thing, so the page does not claim two addresses;
//   - the canonical is in the sitemap, so the two halves of the same claim
//     about which URLs exist cannot drift apart.
import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";

const dist = new URL("../dist/", import.meta.url).pathname;

async function htmlFiles(dir) {
  const found = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) found.push(...(await htmlFiles(path)));
    else if (entry.name.endsWith(".html")) found.push(path);
  }
  return found;
}

function canonicalOf(html) {
  return html.match(/<link[^>]+rel="canonical"[^>]+href="([^"]+)"/i)?.[1] ?? null;
}

function ogUrlOf(html) {
  return html.match(/<meta[^>]+property="og:url"[^>]+content="([^"]+)"/i)?.[1] ?? null;
}

// `https://onebox.run` and `https://onebox.run/` are the same address, and the
// sitemap writes the first while the canonical writes the second. Comparing
// them literally would fail the site's own landing page.
function key(url) {
  return url.replace(/\/$/, "");
}

let pages = [];
try {
  pages = await htmlFiles(dist);
} catch (error) {
  console.error(`check-canonical: cannot read ${dist} — run \`npm run build\` first`);
  console.error(String(error));
  process.exit(1);
}

if (pages.length === 0) {
  console.error("check-canonical: the build produced no HTML, which cannot be right");
  process.exit(1);
}

// The 404 is served for addresses that do not exist, so it is the one page the
// sitemap must not list and the one whose canonical proves nothing.
const NOT_FOUND = join(dist, "404.html");

const failures = [];
const canonicals = new Map();
for (const page of pages) {
  const name = page.replace(dist, "");
  const html = await readFile(page, "utf8");
  const canonical = canonicalOf(html);
  const ogUrl = ogUrlOf(html);

  if (canonical === null) {
    failures.push(`${name}: no <link rel="canonical">`);
    continue;
  }
  if (canonical.endsWith(".html")) {
    failures.push(`${name}: canonical is ${canonical}, which redirects to the extensionless path`);
  }
  if (ogUrl !== canonical) {
    failures.push(`${name}: og:url is ${ogUrl ?? "absent"} but the canonical is ${canonical}`);
  }
  if (page !== NOT_FOUND) canonicals.set(key(canonical), name);
}

let sitemap;
try {
  sitemap = await readFile(join(dist, "sitemap-0.xml"), "utf8");
} catch {
  console.error("check-canonical: dist/sitemap-0.xml is missing, so nothing tells a crawler these pages exist");
  process.exit(1);
}
const listed = new Set(
  [...sitemap.matchAll(/<loc>([^<]+)<\/loc>/g)].map((match) => key(match[1])),
);

for (const [canonical, name] of canonicals) {
  if (!listed.has(canonical)) {
    failures.push(`${name}: canonical ${canonical} is not in the sitemap`);
  }
}

if (failures.length > 0) {
  console.error("check-canonical: pages whose canonical URL is not the one the site publishes:");
  for (const failure of failures) console.error(`  ${failure}`);
  console.error("check-canonical: see src/starlight-route-data.ts, which rewrites these tags");
  process.exit(1);
}

console.log(`check-canonical: ${pages.length} page(s), canonical matches og:url and the sitemap`);
