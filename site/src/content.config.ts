import { defineCollection } from "astro:content";
// `z` re-exported from `astro:content` is deprecated in Astro 7; the bundled
// copy is the supported path and keeps a single zod version in the graph.
import { z } from "astro/zod";
import { docsLoader } from "@astrojs/starlight/loaders";
import { docsSchema } from "@astrojs/starlight/schema";

// Two fields exist for agents rather than readers.
//
// `summary` is the one-line description that appears beside the page in
// llms.txt. Without it the index is a list of titles, which tells an agent
// what a page is called but not whether it is worth fetching.
//
// `read_when` states the situations in which this page is the right one. It is
// retrieval metadata authored by the person who knows the answer, instead of
// inferred from the prose by whatever is doing the retrieving.
//
// `status` is the honesty marker. `shipped` is what the current binary does.
// `schema-only` is accepted by the loader and published in the JSON Schema but
// not yet executable. `intent-only` is validated and planned but nothing
// continuous runs for it. A page that claims more than the binary delivers is
// the failure this field exists to prevent.
export const collections = {
  docs: defineCollection({
    loader: docsLoader(),
    schema: docsSchema({
      extend: z.object({
        summary: z.string().max(240).optional(),
        read_when: z.array(z.string()).optional(),
        status: z
          .enum(["shipped", "schema-only", "intent-only"])
          .default("shipped"),
        generated: z.boolean().default(false),
      }),
    }),
  }),
};
