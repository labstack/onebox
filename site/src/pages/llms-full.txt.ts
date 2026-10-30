import type { APIRoute } from "astro";
import { getCollection } from "astro:content";
import { toPlainMarkdown } from "../lib/markdown";

// Every page, concatenated, for an agent that would rather make one request
// than thirty. Ordered the way a reader would meet the material, because an
// agent reading top to bottom should meet the ownership boundary before it
// meets a field table.
//
// The landing page and docs home share the "index" group, listed explicitly
// so neither is dropped from the documentation indexes.
const ROOT = "index";
const ORDER = [ROOT, "get-started", "guides", "reference", "concepts", "privacy"];

function groupOf(id: string): string {
  if (id === "docs") return ROOT;
  return id.replace(/^docs\//, "").split("/")[0] ?? ROOT;
}

export const GET: APIRoute = async () => {
  const docs = await getCollection("docs");

  const unlisted = [
    ...new Set(docs.map((doc) => groupOf(doc.id))),
  ].filter((group) => !ORDER.includes(group));
  if (unlisted.length > 0) {
    throw new Error(
      `llms-full.txt claims to be complete but would omit: ${unlisted.join(", ")}. ` +
        `Add them to ORDER in src/pages/llms-full.txt.ts.`,
    );
  }

  const ranked = [...docs].sort((a, b) => {
    const ga = ORDER.indexOf(groupOf(a.id));
    const gb = ORDER.indexOf(groupOf(b.id));
    return ga === gb ? a.id.localeCompare(b.id) : ga - gb;
  });

  const parts = [
    "# Onebox — complete documentation",
    "",
    "Production operations for an application intentionally running on one server.",
    "Generated from the documentation source. Sections marked SCHEMA ONLY are",
    "accepted by the loader and published in the JSON Schema but are not yet",
    "executable behaviour.",
    "",
  ];

  for (const doc of ranked) {
    parts.push("---", "");
    parts.push(`# ${doc.data.title}`, "");
    parts.push(`Path: /${doc.id}`);
    if (doc.data.status !== "shipped") {
      parts.push(`Status: ${doc.data.status.toUpperCase().replace("-", " ")}`);
    }
    if (doc.data.summary) parts.push(`Summary: ${doc.data.summary}`);
    parts.push("", toPlainMarkdown(doc.body ?? ""), "");
  }

  return new Response(parts.join("\n"), {
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
};
