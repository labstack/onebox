// Syntax colours for the Terminal theme.
//
// Expressive Code takes its frame from Starlight's grey tokens but its syntax
// palette is independent, so without this the code blocks kept shipping GitHub's
// blue and purple inside a green-grey frame — the one element a theme called
// "Terminal" exists to dress, and the only thing on the page speaking a
// different language.
//
// The palette is deliberately narrow. A real terminal does not colour twelve
// token classes; it distinguishes what you typed from what came back. So:
// literals and strings carry the phosphor, keywords and structure sit in the
// body colour, and comments drop back. Anything not named here inherits the
// foreground, which is the point — this highlights less than a code theme
// usually does, on purpose.

const dark = {
  fg: "#b9c7b4",
  bg: "#0d100d",
  dim: "#87957f",
  bright: "#e2ece0",
  phosphor: "#8fd67a",
  deep: "#4f9a3c",
};

const light = {
  fg: "#353d33",
  bg: "#f6f8f4",
  dim: "#5b6656",
  bright: "#12160f",
  phosphor: "#2f6b22",
  deep: "#1a3d13",
};

function theme(name, type, c) {
  return {
    name,
    type,
    colors: {
      "editor.background": c.bg,
      "editor.foreground": c.fg,
    },
    settings: [
      { scope: ["comment", "punctuation.definition.comment"], settings: { foreground: c.dim, fontStyle: "italic" } },
      // What a command or a config actually carries: values.
      { scope: ["string", "string.quoted", "meta.embedded.line", "constant.other.symbol"], settings: { foreground: c.phosphor } },
      { scope: ["constant.numeric", "constant.language", "constant.language.boolean"], settings: { foreground: c.phosphor } },
      // Keys and identifiers read as the thing being described.
      { scope: ["entity.name.tag", "support.type.property-name", "meta.object-literal.key", "variable.other.readwrite"], settings: { foreground: c.bright } },
      { scope: ["entity.name.function", "support.function"], settings: { foreground: c.bright } },
      // Structure stays quiet — a terminal does not colour its own punctuation.
      { scope: ["keyword", "storage", "storage.type", "keyword.operator", "punctuation"], settings: { foreground: c.fg } },
      { scope: ["variable.parameter", "entity.name.type", "support.class"], settings: { foreground: c.fg } },
      { scope: ["invalid", "invalid.illegal"], settings: { foreground: c.deep } },
    ],
  };
}

export const oneboxCodeDark = theme("onebox-terminal-dark", "dark", dark);
export const oneboxCodeLight = theme("onebox-terminal-light", "light", light);
