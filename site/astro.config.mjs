// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import { rehypeTableWrap } from "./src/plugins/rehype-table-wrap.mjs";
import { oneboxCodeDark, oneboxCodeLight } from "./src/styles/code-theme.mjs";

// The site URL is what makes llms.txt and the Markdown alternates absolute.
// An agent that resolves a relative link against the wrong origin fetches
// nothing, so this is set rather than inferred.
const SITE = process.env.SITE_URL ?? "https://onebox.run";

export default defineConfig({
  site: SITE,
  trailingSlash: "never",
  build: { format: "file" },
  markdown: { rehypePlugins: [rehypeTableWrap] },
  integrations: [
    starlight({
      title: "Onebox",
      // The separator between a page's title and the site's: "Install · Onebox".
      // It was added for the landing page, which is titled "Onebox" like the
      // site and so read as "Onebox | Onebox" — that it still read as
      // "Onebox · Onebox" afterwards is the part the original note missed. The
      // landing page now sets its own title outright and does not pass through
      // here at all; every other page does, which is what this is for.
      titleDelimiter: "·",
      description:
        "Production operations for an application intentionally running on one server.",
      // The project tagline, in the same words as the GitHub description and the
      // landing page's title. Starlight renders this only on a splash page that
      // does not supply its own hero tagline; index.mdx supplies one, so today
      // this has no output. It is kept in step anyway, because the day a second
      // splash page exists is not the day to discover the tagline drifted.
      tagline: "Plan-before-apply deploys. Zero downtime. One box.",
      social: [
        {
          icon: "github",
          label: "GitHub",
          href: "https://github.com/labstack/onebox",
        },
      ],
      editLink: {
        baseUrl:
          "https://github.com/labstack/onebox/edit/main/site/",
      },
      // Expressive Code's syntax palette is independent of Starlight's grey
      // tokens, so without naming themes here the code blocks keep GitHub's
      // colours inside a themed frame.
      expressiveCode: {
        themes: [oneboxCodeDark, oneboxCodeLight],
        styleOverrides: { borderColor: "var(--sl-color-gray-5)" },
      },
      customCss: ["./src/styles/onebox.css"],
      components: {
        // Advertises the Markdown alternate for the current page, so an agent
        // that reads the HTML head never has to parse the HTML body.
        Head: "./src/components/Head.astro",
        // Starlight renders Hero in place of a page title for any page that
        // declares `hero` in its frontmatter. The landing page is the only one
        // that does, so this override is effectively scoped to it — but the
        // registration is global, and a second splash page with a hero would
        // get the landing page's layout rather than Starlight's.
        Hero: "./src/components/landing/Hero.astro",
        // Starlight's own footer, minus the documentation affordances on the
        // splash page.
        Footer: "./src/components/Footer.astro",
        // Adds the site footer band below the content, and lets the main frame
        // take the slack so it sits at the bottom of a short page.
        PageFrame: "./src/components/PageFrame.astro",
      },
      lastUpdated: true,
      pagination: true,
      favicon: "/favicon.svg",
      sidebar: [
        {
          label: "Start here",
          items: [
            { label: "What Onebox is", slug: "start/what-onebox-is" },
            { label: "Install", slug: "start/install" },
            { label: "Your first deploy", slug: "start/first-deploy" },
            { label: "Reading the file back", slug: "start/reading-it-back" },
          ],
        },
        {
          label: "Guides",
          items: [
            { label: "Add a database", slug: "guides/add-a-database" },
            { label: "Back up a database", slug: "guides/back-up-a-database" },
            { label: "Handle secrets", slug: "guides/handle-secrets" },
            { label: "Run migrations safely", slug: "guides/run-migrations" },
            { label: "Schedule a job", slug: "guides/schedule-a-job" },
            { label: "Roll back a release", slug: "guides/roll-back" },
            { label: "Adopt an existing Compose file", slug: "guides/adopt-compose" },
            { label: "Eject", slug: "guides/eject" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "Project file", slug: "reference/project-file" },
            {
              label: "Project file fields",
              items: [{ autogenerate: { directory: "reference/fields" } }],
            },
            { label: "CLI commands", slug: "reference/cli" },
            { label: "Error codes", slug: "reference/errors" },
            { label: "Service drivers", slug: "reference/drivers" },
            { label: "Policies", slug: "reference/policies" },
          ],
        },
        {
          label: "Explanation",
          items: [
            { label: "The ownership boundary", slug: "explanation/ownership-boundary" },
            { label: "Evidence, not declaration", slug: "explanation/evidence-not-declaration" },
            { label: "Why Compose is generated", slug: "explanation/generated-compose" },
            { label: "What Onebox refuses", slug: "explanation/what-onebox-refuses" },
            { label: "The safety envelope", slug: "explanation/safety-envelope" },
          ],
        },
        {
          label: "Status",
          items: [{ label: "Shipped vs proposed", slug: "status/capabilities" }],
        },
      ],
    }),
  ],
});
