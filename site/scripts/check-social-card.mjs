// Assert that the og:image every page advertises is actually in the build, and
// is the size the same head claims it is.
//
// Starlight emits `twitter:card: summary_large_image` and no image to pair with
// it, so `src/components/Head.astro` supplies one. That tag is written by hand
// against a file in `public/`, and nothing else connects the two: rename or drop
// `social-card.png` and the build still succeeds while every page points a
// crawler at a 404 — a failure that shows up in other people's link previews
// long before it shows up in anything this repository builds or serves.
//
// The dimensions are checked too, because `og:image:width` and `og:image:height`
// are hand-typed. A card re-rendered at another size with the numbers left
// behind makes consumers reserve the wrong box, which is the whole reason those
// tags exist.
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

function meta(html, attribute, name) {
  const pattern = new RegExp(
    `<meta[^>]+${attribute}="${name}"[^>]+content="([^"]+)"`,
    "i",
  );
  return html.match(pattern)?.[1] ?? null;
}

// The IHDR chunk is the first one in every PNG and holds the dimensions as two
// big-endian 32-bit integers, so the header alone answers this without a decoder.
function pngSize(bytes) {
  const signature = "89504e470d0a1a0a";
  if (bytes.subarray(0, 8).toString("hex") !== signature) return null;
  if (bytes.subarray(12, 16).toString("ascii") !== "IHDR") return null;
  return { width: bytes.readUInt32BE(16), height: bytes.readUInt32BE(20) };
}

let pages = [];
try {
  pages = await htmlFiles(dist);
} catch (error) {
  console.error(`check-social-card: cannot read ${dist} — run \`npm run build\` first`);
  console.error(String(error));
  process.exit(1);
}

const missingTag = [];
const declared = new Map();
for (const page of pages) {
  const html = await readFile(page, "utf8");
  const href = meta(html, "property", "og:image");
  if (href === null) {
    missingTag.push(page.replace(dist, ""));
    continue;
  }
  const width = meta(html, "property", "og:image:width");
  const height = meta(html, "property", "og:image:height");
  declared.set(href, { width, height });
}

if (missingTag.length > 0) {
  console.error("check-social-card: pages built without an og:image, so they unfurl as an empty large card:");
  for (const page of missingTag) console.error(`  ${page}`);
  process.exit(1);
}

if (declared.size === 0) {
  console.error("check-social-card: the build produced no HTML, which cannot be right");
  process.exit(1);
}

for (const [href, size] of declared) {
  // The tag is written by hand, so an author could reasonably make it relative.
  // Open Graph consumers do not resolve those, and neither does this check —
  // saying so beats throwing a URL parse error at whoever runs the build.
  let path;
  try {
    path = new URL(href).pathname;
  } catch {
    console.error(`check-social-card: og:image is "${href}", which is not an absolute URL — consumers cannot resolve it`);
    process.exit(1);
  }
  let bytes;
  try {
    bytes = await readFile(join(dist, path));
  } catch {
    console.error(`check-social-card: ${href} is advertised by every page but ${path} is not in the build`);
    process.exit(1);
  }

  const actual = pngSize(bytes);
  if (actual === null) {
    console.error(`check-social-card: ${path} is not a PNG, so consumers that trust the tag will show nothing`);
    process.exit(1);
  }
  if (String(actual.width) !== size.width || String(actual.height) !== size.height) {
    console.error(
      `check-social-card: ${path} is ${actual.width}x${actual.height}, but the head declares ${size.width}x${size.height}`,
    );
    console.error("check-social-card: re-render with `just social-card`, or correct the tags in src/components/Head.astro");
    process.exit(1);
  }
}

console.log(`check-social-card: ${declared.size} og:image target(s), present and correctly sized`);
