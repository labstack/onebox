// Turn page source into Markdown an agent can read without knowing MDX.
//
// The pages are MDX, so their source carries three things that are noise to
// anything that is not Astro: ESM `import` lines, Starlight component tags, and
// the `:::` directive syntax. Serving that raw under `text/markdown` — which is
// what the alternates did — means `/start/install.md` opens with an import
// statement and `<Steps>`, and the caution blocks that carry the shipped versus
// proposed distinction arrive as literal colons.
//
// The directives are converted rather than stripped, because their content is
// exactly the part an agent most needs: `:::caution[Not yet executable]` becomes
// a blockquote that still says so.
export function toPlainMarkdown(source: string): string {
  const out: string[] = [];

  let inFence = false;
  let inDirective = false;
  let directiveIndent = "";

  for (const line of source.split("\n")) {
    const fence = line.trimStart().startsWith("```");
    if (fence) inFence = !inFence;

    if (!inFence) {
      // ESM imports at the top of an MDX file.
      if (/^import\s.+\sfrom\s+["'].+["'];?\s*$/.test(line)) continue;

      // The generator's own marker, and any other MDX comment.
      if (/^\s*\{\/\*.*\*\/\}\s*$/.test(line)) continue;

      // `:::note[Title]` / `:::caution` open, `:::` close.
      const open = line.match(/^(\s*):::(\w+)(?:\[(.+?)\])?\s*$/);
      if (open) {
        directiveIndent = open[1] ?? "";
        const kind = open[2] ?? "note";
        const title = open[3] ?? kind.charAt(0).toUpperCase() + kind.slice(1);
        out.push(`${directiveIndent}> **${title}**`, `${directiveIndent}>`);
        inDirective = true;
        continue;
      }
      if (inDirective && line.trim() === ":::") {
        inDirective = false;
        directiveIndent = "";
        out.push("");
        continue;
      }
      if (inDirective) {
        const content = line.startsWith(directiveIndent)
          ? line.slice(directiveIndent.length)
          : line;
        out.push(
          content.trim() === ""
            ? `${directiveIndent}>`
            : `${directiveIndent}> ${content}`,
        );
        continue;
      }
    }

    out.push(line);
  }

  return stripComponentTags(out.join("\n"));
}

// Starlight components carry their text as children, so the tags are removed and
// the prose between them kept. Tab labels are retained because they provide the
// context for otherwise indistinguishable command blocks. Self-closing components
// carry nothing a reader needs and go entirely.
function stripComponentTags(source: string): string {
  const fences = source.split(/(```[\s\S]*?```)/g);
  return fences
    .map((chunk, index) =>
      // Odd indices are fenced code, which is left exactly as authored.
      index % 2 === 1
        ? chunk
        : chunk
            .replace(
              /<TabItem\b[^>]*\blabel=["']([^"']+)["'][^>]*>/g,
              "\n**$1**\n",
            )
            .replace(/<([A-Z]\w*)\b[^>]*\/>/g, "")
            .replace(/<\/?([A-Z]\w*)\b[^>]*>/g, "")
            .replace(/\n{3,}/g, "\n\n"),
    )
    .join("");
}
