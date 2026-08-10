import type { APIRoute } from "astro";
import { getCollection } from "astro:content";

// The index an agent reads first.
//
// A list of titles tells an agent what a page is called. It does not tell it
// whether the page is worth fetching, which is the only question the agent has.
// So every entry carries the author's own one-line summary, and the sections
// that are not yet executable say so in the index rather than in the page the
// agent may never open.
//
// The landing page is its own group: splitting on the first path segment gives
// it "index", which an ORDER list of directory names does not contain, so it was
// dropped from an index whose own heading promises every page.
const ROOT = "index";
const ORDER = [ROOT, "start", "guides", "reference", "explanation", "status"];

const GROUP_TITLES: Record<string, string> = {
  [ROOT]: "Overview",
  start: "Start here",
  guides: "Guides",
  reference: "Reference",
  explanation: "Explanation",
  status: "Status",
};

// Keyed by the status union rather than by string, so adding a fourth status is
// a compile error here instead of a page quietly advertised to an agent without
// its honesty marker.
type PageStatus = "shipped" | "schema-only" | "intent-only";

const STATUS_NOTE: Record<Exclude<PageStatus, "shipped">, string> = {
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
    "Production operations for the sole Onebox application on one server.",
    "You describe what your application is in `ob.yml`; Onebox derives the Compose",
    "runtime, names, routing, health gating, the proxy, and supporting services.",
    "It connects over SSH. There is no deployment agent on the host.",
    "",
    "> Use this file as a map of the Onebox documentation. Fetch any page as",
    "> Markdown by appending `.md` to its URL. The CLI is the interface for people",
    "> and agents alike: each executable leaf has a closed output protocol,",
    "> errors are typed, and lifecycle mutations are idempotent under retry.",
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
    "- Structured output (`--output human|json|ndjson`) is carried by these commands only: `ob abort`, `ob approve`, `ob audit`, `ob bootstrap`, `ob canonical`, `ob deploy`, `ob destroy`, `ob doctor`, `ob eject`, `ob exec`, `ob init`, `ob job plan`, `ob job run`, `ob logs`, `ob plan`, `ob preflight`, `ob preview`, `ob proxy apply`, `ob resume`, `ob rollback`, `ob schema`, `ob secrets edit`, `ob secrets list`, `ob secrets push`, `ob service apply`, `ob status`, `ob validate`, `ob version`. The permitted JSON/NDJSON modes differ by command; see the policy matrix.",
    "- Read-only and safe at any time: `ob validate`, `ob canonical`, `ob preview`, `ob schema`, `ob status`, `ob doctor`, `ob audit`, `ob logs`, `ob plan`, `ob preflight`.",
    "- Lifecycle operation commands: `ob deploy`, `ob rollback`, `ob resume`, `ob abort`, `ob bootstrap`, `ob destroy`, `ob job run`, `ob service apply`, `ob proxy apply`, `ob secrets push`; each enforces its own plan, confirmation, lock, and recovery requirements.",
    "- `ob exec --reason ...` is an audited escape hatch. Nothing it changes belongs to a release; the journal stores only safe invocation metadata and a command digest, never command or output bytes.",
    "- `ob approve` records a short-lived local confirmation bound to one exact plan. Its digest detects modification; it is not authenticated identity or an independently issued capability.",
    "- A failure carries a stable `code` and separates read-only `diagnostic_command`, workflow `next_command`, and mutation-capable `resolving_command` guidance. Branch on the code, not the sentence. The full catalogue is at `/reference/errors.md`.",
    "",
    "See `/reference/policies.md` for the schema identity of every structured document.",
    "",
  ];

  // Silently dropping a section is how a whole directory of documentation
  // becomes invisible to every agent while the site builds clean and the
  // sidebar still shows it. Fail the build instead.
  const groups = new Set(docs.map((doc) => doc.id.split("/")[0] ?? ROOT));
  const unlisted = [...groups].filter((group) => !ORDER.includes(group));
  if (unlisted.length > 0) {
    throw new Error(
      `llms.txt would omit these documentation sections: ${unlisted.join(", ")}. ` +
        `Add them to ORDER and GROUP_TITLES in src/pages/llms.txt.ts.`,
    );
  }

  const unsummarised = docs.filter(
    (doc) => !doc.data.summary && !doc.data.description,
  );
  if (unsummarised.length > 0) {
    throw new Error(
      `these pages would be indexed with no summary, which is the one thing an ` +
        `agent needs to decide whether to fetch them: ${unsummarised
          .map((doc) => doc.id || "index")
          .join(", ")}`,
    );
  }

  const byGroup = new Map<string, typeof docs>();
  for (const doc of docs) {
    const group = doc.id.split("/")[0] ?? ROOT;
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
      const summary = entry.data.summary ?? entry.data.description;
      const status = entry.data.status as PageStatus;
      const note = status === "shipped" ? "" : STATUS_NOTE[status];
      const path = entry.id;
      lines.push(`- [${title}](${origin}/${path}.md): ${summary}${note}`);

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
