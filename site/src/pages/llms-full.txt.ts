import type { APIRoute } from "astro";
import { getCollection } from "astro:content";

// Every page, concatenated, for an agent that would rather make one request
// than thirty. Ordered the way a reader would meet the material, because an
// agent reading top to bottom should meet the ownership boundary before it
// meets a field table.
const ORDER = ["start", "guides", "reference", "explanation", "status"];

export const GET: APIRoute = async () => {
  const docs = await getCollection("docs");

  const ranked = docs
    .filter((doc) => ORDER.includes(doc.id.split("/")[0] ?? ""))
    .sort((a, b) => {
      const ga = ORDER.indexOf(a.id.split("/")[0] ?? "");
      const gb = ORDER.indexOf(b.id.split("/")[0] ?? "");
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
    parts.push("", doc.body ?? "", "");
  }

  return new Response(parts.join("\n"), {
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
};
