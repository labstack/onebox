import assert from "node:assert/strict";
import { test } from "node:test";
import { toPlainMarkdown } from "../src/lib/markdown.ts";

test("documentation cards retain their section titles and links in Markdown", () => {
  const source = `import { Card, CardGrid } from "@astrojs/starlight/components";

<CardGrid>
  <Card title="Get started">

- [Install](/docs/get-started/install)

  </Card>
</CardGrid>`;
  const markdown = toPlainMarkdown(source);
  assert(markdown.includes("### Get started"));
  assert(markdown.includes("- [Install](/docs/get-started/install)"));
  assert.doesNotMatch(markdown, /import|<\/?Card/);
});

test("Markdown conversion preserves component examples inside code fences", () => {
  const source = '```mdx\n<Card title="Example">\n\n[Install](/docs/get-started/install)\n\n</Card>\n```';
  assert.equal(toPlainMarkdown(source), source);
});
