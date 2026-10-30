// Verify the deployed URL contract: every docs page lives under /docs and has
// a Markdown export listed in both LLM indexes.
import assert from "node:assert/strict";
import { readFile, readdir } from "node:fs/promises";

const dist = new URL("../dist/", import.meta.url);

const llms = await readFile(new URL("llms.txt", dist), "utf8");
const full = await readFile(new URL("llms-full.txt", dist), "utf8");
const home = await readFile(new URL("docs.html", dist), "utf8");
assert(home.includes("<title>Documentation · Onebox</title>"), "docs.html: incorrect title");
for (const title of ["Get started", "Guides", "Reference", "Concepts"]) {
  assert(llms.includes(`## ${title}\n`), `llms.txt: missing ${title} section`);
}

let pages = 0;
async function checkPages(dir, prefix = "") {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = `${prefix}${entry.name}`;
    if (entry.isDirectory()) {
      await checkPages(new URL(`${entry.name}/`, dir), `${path}/`);
    } else if (path.endsWith(".html") && path !== "404.html") {
      if (path === "index.html" || path === "privacy.html") continue;
      const url = `/${path.slice(0, -5)}`;
      assert(url === "/docs" || url.startsWith("/docs/"), `${url}: documentation escaped /docs`);
      const markdown = await readFile(new URL(`${path.slice(0, -5)}.md`, dist), "utf8");
      assert(markdown.trim(), `${url}: empty Markdown export`);
      const html = await readFile(new URL(entry.name, dir), "utf8");
      const fileLink = html.match(/href="(\/[^"#?]*\.html)"/)?.[1];
      assert(!fileLink, `${url}: links to ${fileLink}; links must use the extensionless path`);
      const alternate = html.match(/<link[^>]+type="text\/markdown"[^>]+href="([^"]+)"/)?.[1];
      assert(alternate, `${url}: missing Markdown alternate`);
      assert.equal(new URL(alternate, "https://onebox.run").pathname, `${url}.md`,
        `${url}: incorrect Markdown alternate`);
      assert(llms.includes(`${url}.md)`), `${url}: missing from llms.txt`);
      assert(full.includes(`Path: ${url}\n`), `${url}: missing from llms-full.txt`);
      pages++;
    }
  }
}
await checkPages(dist);
assert(pages > 0, "no docs pages found");
console.log(`check-docs-urls: ${pages} docs pages and their Markdown exports verified`);
