import type { APIRoute, GetStaticPaths } from "astro";
import { getCollection, type CollectionEntry } from "astro:content";
import { toPlainMarkdown } from "../lib/markdown";

// Clean Markdown for every page, at `<path>.md`.
//
// This is the same source the site renders, with the agent-facing frontmatter
// kept and the presentational frontmatter dropped. It exists so an agent never
// has to reconstruct prose from rendered HTML, and so what an agent reads and
// what a person reads cannot drift apart: there is one source.
export const getStaticPaths: GetStaticPaths = async () => {
  const docs = await getCollection("docs");
  return docs.map((entry) => ({
    // The landing page's id is already "index", so no path needs inventing here;
    // every page has a Markdown alternate at a real path.
    params: { slug: entry.id },
    props: { entry },
  }));
};

export const GET: APIRoute = ({ props }) => {
  const { entry } = props as { entry: CollectionEntry<"docs"> };
  const data = entry.data;

  const front: string[] = ["---", `title: ${JSON.stringify(data.title)}`];
  if (data.summary) front.push(`summary: ${JSON.stringify(data.summary)}`);
  if (data.description && data.description !== data.summary) {
    front.push(`description: ${JSON.stringify(data.description)}`);
  }
  front.push(`status: ${data.status}`);
  if (data.generated) front.push("generated: true");
  if (data.read_when?.length) {
    front.push("read_when:");
    for (const when of data.read_when) front.push(`  - ${JSON.stringify(when)}`);
  }
  front.push("---", "");

  // A body that failed to load would otherwise be served as a valid 200 with
  // frontmatter and nothing else, which an agent cannot tell from a short page.
  const body = toPlainMarkdown(entry.body ?? "");
  if (!body.trim()) {
    throw new Error(
      `${entry.id || "index"} has no body: its .md alternate would serve frontmatter only`,
    );
  }

  return new Response(front.join("\n") + body, {
    headers: { "Content-Type": "text/markdown; charset=utf-8" },
  });
};
