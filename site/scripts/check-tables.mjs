// Assert that every table in the built site sits inside a scroll container.
//
// Generated reference pages have tables far wider than a phone. Unwrapped,
// they are clipped at the content edge with no way to reach the rest — the page
// correctly refuses to scroll sideways, and the last two columns simply cannot
// be read.
//
// This runs against `dist/` rather than inside the rehype plugin on purpose. A
// plugin can only check tables it was handed, so it cannot notice the failure
// that matters most: a Starlight upgrade taking over the markdown config, after
// which the plugin never runs at all and every table ships bare. Reading the
// emitted HTML is the only vantage point from which that is visible.
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

// Walks the raw HTML rather than parsing it: the only question is whether the
// nearest enclosing element of each <table> carries the wrapper class, and the
// build emits the wrapper immediately around the table.
function unwrappedTables(html) {
  let count = 0;
  let from = 0;
  for (;;) {
    const at = html.indexOf("<table", from);
    if (at === -1) return count;
    const before = html.slice(Math.max(0, at - 200), at);
    const opensWrapper = before.lastIndexOf('class="table-wrap"');
    const closesDiv = before.lastIndexOf("</div>");
    if (opensWrapper === -1 || closesDiv > opensWrapper) count += 1;
    from = at + 6;
  }
}

let pages = [];
try {
  pages = await htmlFiles(dist);
} catch (error) {
  console.error(`check-tables: cannot read ${dist} — run \`npm run build\` first`);
  console.error(String(error));
  process.exit(1);
}

if (pages.length === 0) {
  console.error("check-tables: the build produced no HTML, which cannot be right");
  process.exit(1);
}

let tables = 0;
const offenders = [];
for (const page of pages) {
  const html = await readFile(page, "utf8");
  const total = (html.match(/<table/g) ?? []).length;
  tables += total;
  const bare = unwrappedTables(html);
  if (bare > 0) offenders.push(`${page.replace(dist, "")}: ${bare} of ${total}`);
}

if (offenders.length > 0) {
  console.error(
    "check-tables: tables outside a .table-wrap container would be clipped on a phone:",
  );
  for (const offender of offenders) console.error(`  ${offender}`);
  process.exit(1);
}

// Zero tables would mean the reference pages stopped rendering their tables,
// which is a louder failure than an unwrapped one.
if (tables === 0) {
  console.error("check-tables: no tables found in the built site at all");
  process.exit(1);
}

console.log(`check-tables: ${tables} table(s) across ${pages.length} page(s), all wrapped`);
