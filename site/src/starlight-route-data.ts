import { defineRouteMiddleware } from "@astrojs/starlight/route-data";

// Strips the `.html` Astro puts in the canonical URL.
//
// Starlight builds the canonical from `Astro.url.pathname`, and under
// `build.format: "preserve"` that pathname is the file it wrote to disk, so the tag
// read `https://onebox.run/docs/get-started/install.html`. Nothing else on the site uses
// that form: the sitemap lists `/docs/get-started/install`, every internal link points at
// `/docs/get-started/install`, `llms.txt` points at `/docs/get-started/install.md`, and Cloudflare
// Pages — where this site is published — serves the extensionless path and 301s
// the `.html` one to it. The single tag whose job is to state the page's real
// address was naming a redirect, on every page.
//
// No Starlight option strips it: `formatCanonical` only adjusts a trailing
// slash. Navigation links need no correction, because "preserve" (unlike
// "file") makes Starlight emit them without the extension.
//
// The tags are rewritten in place rather than appended to, because two
// `<link rel="canonical">` elements with different hrefs are worse than one
// wrong href: a crawler that sees the pair discards both and picks a canonical
// on its own.
function withoutHtmlExtension(href: string): string {
  const url = new URL(href);
  if (url.pathname === "/index.html") {
    url.pathname = "/";
  } else if (url.pathname.endsWith(".html")) {
    url.pathname = url.pathname.slice(0, -".html".length);
  }
  return url.href;
}

export const onRequest = defineRouteMiddleware((context) => {
  for (const tag of context.locals.starlightRoute.head) {
    // `og:url` is generated from the same string as the canonical, so a fix
    // that touched only the `<link>` would leave the two disagreeing about
    // which URL the page is.
    if (
      tag.tag === "link" &&
      tag.attrs?.rel === "canonical" &&
      typeof tag.attrs.href === "string"
    ) {
      tag.attrs.href = withoutHtmlExtension(tag.attrs.href);
    } else if (
      tag.tag === "meta" &&
      tag.attrs?.property === "og:url" &&
      typeof tag.attrs.content === "string"
    ) {
      tag.attrs.content = withoutHtmlExtension(tag.attrs.content);
    }
  }
});
