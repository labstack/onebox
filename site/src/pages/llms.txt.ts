import type { APIRoute } from "astro";
import { getCollection } from "astro:content";

// The index an agent reads first.
//
// A list of titles tells an agent what a page is called. It does not tell it
// whether the page is worth fetching, which is the only question the agent has.
// So every entry carries the author's own one-line summary, and the sections
// that are not yet executable say so in the index rather than in the page the
// agent may never open.
const ORDER = ["start", "guides", "reference", "explanation", "status"];

const GROUP_TITLES: Record<string, string> = {
  start: "Start here",
  guides: "Guides",
  reference: "Reference",
  explanation: "Explanation",
  status: "Status",
};

const STATUS_NOTE: Record<string, string> = {
  "schema-only":
    " [SCHEMA ONLY: the loader validates this and it is published in the JSON Schema, but the behaviour is not yet executable]",
  "intent-only":
    " [INTENT ONLY: validated and carried into plans, but nothing continuous runs for it]",
};

export const GET: APIRoute = async ({ site }) => {
  const origin = (site ?? new URL("https://onebox.run")).origin;
  const docs = await getCollection("docs");

  const lines: string[] = [
    "# Onebox",
    "",
    "Production operations for an application intentionally running on one server.",
    "You describe what your application is in `ob.yml`; Onebox derives the Compose",
    "runtime, names, routing, health gating, the proxy, and supporting services.",
    "It connects over SSH. There is no deployment agent on the host.",
    "",
    "> Use this file as a map of the Onebox documentation. Fetch any page as clean",
    "> Markdown by appending `.md` to its URL. The CLI is the interface for people",
    "> and agents alike: every command carries `--output json|ndjson`, errors are",
    "> typed, and mutations are idempotent under retry.",
    "",
    "## Agent Resources",
    "",
    `- [Markdown page export](${origin}/start/first-deploy.md): Append \`.md\` to any docs page URL for clean Markdown.`,
    `- [Full documentation text](${origin}/llms-full.txt): Every page concatenated, for one-shot ingestion.`,
    `- [Project file JSON Schema](${origin}/onebox.run-v1.schema.json): The machine contract the loader enforces.`,
    `- [Sitemap](${origin}/sitemap-index.xml): Crawler URL index.`,
    "",
    "## Operating Onebox from an agent",
    "",
    "- Read-only and safe at any time: `ob validate`, `ob canonical`, `ob preview`, `ob schema`, `ob status`, `ob doctor`, `ob audit`, `ob logs`, `ob plan`.",
    "- Mutating and approval-gated: `ob deploy`, `ob rollback`, `ob resume`, `ob abort`, `ob bootstrap`, `ob destroy`, `ob service apply`, `ob proxy apply`, `ob secrets push`.",
    "- `ob approve` writes a grant bound to one exact plan. An agent cannot mint the capability that authorises its own change.",
    "- A failure carries a stable `code`, the path that produced it, and the command that resolves it. Branch on the code, not the sentence.",
    "",
  ];

  const byGroup = new Map<string, typeof docs>();
  for (const doc of docs) {
    const group = doc.id.split("/")[0] ?? "";
    if (!ORDER.includes(group)) continue;
    const bucket = byGroup.get(group) ?? [];
    bucket.push(doc);
    byGroup.set(group, bucket);
  }

  for (const group of ORDER) {
    const entries = (byGroup.get(group) ?? []).sort((a, b) =>
      a.id.localeCompare(b.id),
    );
    if (entries.length === 0) continue;

    lines.push(`## ${GROUP_TITLES[group] ?? group}`, "");
    for (const entry of entries) {
      const title = entry.data.title;
      const summary =
        entry.data.summary ?? entry.data.description ?? "No summary available.";
      const note = STATUS_NOTE[entry.data.status] ?? "";
      lines.push(`- [${title}](${origin}/${entry.id}.md): ${summary}${note}`);

      for (const when of entry.data.read_when ?? []) {
        lines.push(`  - Read when: ${when}`);
      }
    }
    lines.push("");
  }

  return new Response(lines.join("\n"), {
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
};
