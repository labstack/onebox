import type { APIRoute, GetStaticPaths } from "astro";
import { getCollection } from "astro:content";

// Clean Markdown for every page, at `<path>.md`.
//
// This is the same source the site renders, with the agent-facing frontmatter
// kept and the presentational frontmatter dropped. It exists so an agent never
// has to reconstruct prose from rendered HTML, and so what an agent reads and
// what a person reads cannot drift apart: there is one source.
export const getStaticPaths: GetStaticPaths = async () => {
  const docs = await getCollection("docs");
  return docs.map((entry) => ({
    // The landing page's id is empty, which would produce `/.md`. It is named
    // explicitly so every page has a Markdown alternate at a real path.
    params: { slug: entry.id === "" ? "index" : entry.id },
    props: { entry },
  }));
};

export const GET: APIRoute = ({ props }) => {
  const { entry } = props as { entry: any };
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

  return new Response(front.join("\n") + (entry.body ?? ""), {
    headers: { "Content-Type": "text/markdown; charset=utf-8" },
  });
};
