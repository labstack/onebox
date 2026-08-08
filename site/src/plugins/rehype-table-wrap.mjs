/**
 * Wrap every Markdown table in a scrollable container.
 *
 * The generated field reference has tables far wider than a phone. Without a
 * wrapper the table is clipped at the content edge with no way to reach the
 * rest of it: the page correctly refuses to scroll sideways, and the last two
 * columns simply cannot be read. Wrapping moves the overflow onto an element
 * that is allowed to scroll, so the page stays put and the table does not.
 */
export function rehypeTableWrap() {
  return (tree, file) => {
    let tables = 0;
    let wrapped = 0;

    visit(tree, (node, index, parent) => {
      if (!parent || node.tagName !== "table" || index === null) return;
      tables += 1;
      if (parent.type === "element" && parent.properties?.className?.includes?.("table-wrap")) {
        wrapped += 1;
        return;
      }
      parent.children[index] = {
        type: "element",
        tagName: "div",
        properties: {
          className: ["table-wrap"],
          tabIndex: 0,
          role: "region",
          // A `region` with no accessible name is not exposed as a landmark, so
          // the keyboard stop this adds would announce nothing.
          "aria-label": "Reference table, scrollable",
        },
        children: [node],
      };
      wrapped += 1;
    });

    // Silence is the failure mode worth catching: if Starlight starts wrapping
    // tables itself, or an upgrade takes over the markdown config, this becomes
    // a no-op and the wide reference tables are clipped again with nothing said.
    if (tables !== wrapped) {
      const where = file?.path ?? "a page";
      throw new Error(
        `rehype-table-wrap: ${tables - wrapped} table(s) in ${where} were not wrapped; wide reference tables would be clipped`,
      );
    }
  };
}

function visit(node, fn, index = null, parent = null) {
  if (node.type === "element" || node.type === "root") {
    fn(node, index, parent);
    const children = node.children ?? [];
    for (let i = children.length - 1; i >= 0; i--) {
      visit(children[i], fn, i, node);
    }
  }
}

export default rehypeTableWrap;
