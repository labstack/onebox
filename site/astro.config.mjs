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
  // Reserved local-development port; no reverse proxy is used.
  server: { port: 7565 },
  vite: { server: { strictPort: true } },
  trailingSlash: "never",
  build: { format: "preserve" },
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
        SocialIcons: "./src/components/SocialIcons.astro",
      },
      // Starlight derives the canonical URL from the on-disk file name, which
      // under `build.format: "preserve"` carries a `.html` no link, sitemap entry
      // or agent-facing URL on this site uses. See the middleware for why the
      // tags are rewritten rather than re-declared.
      routeMiddleware: "./src/starlight-route-data.ts",
      lastUpdated: true,
      pagination: true,
      favicon: "/favicon.svg",
      sidebar: [
        {
          label: "Get started",
          items: [
            { label: "What Onebox is", slug: "docs/get-started/overview" },
            { label: "Install", slug: "docs/get-started/install" },
            { label: "Your first deploy", slug: "docs/get-started/first-deploy" },
            { label: "Reading the file back", slug: "docs/get-started/reading-it-back" },
          ],
        },
        {
          label: "Guides",
          items: [
            { label: "Add a database", slug: "docs/guides/databases" },
            { label: "Back up a database", slug: "docs/guides/backups" },
            { label: "Handle secrets", slug: "docs/guides/secrets" },
            { label: "Run migrations safely", slug: "docs/guides/migrations" },
            { label: "Schedule a job", slug: "docs/guides/cron-jobs" },
            { label: "Roll back a release", slug: "docs/guides/rollback" },
            { label: "Deploy through a jump host", slug: "docs/guides/jump-host" },
            { label: "Environment variables", slug: "docs/guides/environment-variables" },
            { label: "Adopt an existing Compose file", slug: "docs/guides/compose" },
            { label: "Eject", slug: "docs/guides/eject" },
            { label: "Upgrade to onebox names", slug: "docs/guides/upgrade-to-onebox-names" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "Project file", slug: "docs/reference/project-file" },
            {
              label: "Project file fields",
              collapsed: true,
              items: [{ autogenerate: { directory: "docs/reference/fields" } }],
            },
            { label: "CLI commands", slug: "docs/reference/cli" },
            { label: "Error codes", slug: "docs/reference/errors" },
            { label: "Service drivers", slug: "docs/reference/drivers" },
            { label: "Policies", slug: "docs/reference/policies" },
            { label: "Naming", slug: "docs/reference/naming" },
            { label: "Shipped vs proposed", slug: "docs/reference/capabilities" },
          ],
        },
        {
          label: "Concepts",
          items: [
            { label: "The ownership boundary", slug: "docs/concepts/ownership-boundary" },
            { label: "Evidence, not declaration", slug: "docs/concepts/evidence-not-declaration" },
            { label: "Why Compose is generated", slug: "docs/concepts/generated-compose" },
            { label: "What Onebox refuses", slug: "docs/concepts/what-onebox-refuses" },
            { label: "The safety envelope", slug: "docs/concepts/safety-envelope" },
          ],
        },
      ],
    }),
  ],
});
