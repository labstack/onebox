/**
 * Wrap every Markdown table in a scrollable container.
 *
 * Generated reference pages have tables far wider than a phone. Without a
 * wrapper the table is clipped at the content edge with no way to reach the
 * rest of it: the page correctly refuses to scroll sideways, and the last two
 * columns simply cannot be read. Wrapping moves the overflow onto an element
 * that is allowed to scroll, so the page stays put and the table does not.
 */
// The invariant this plugin exists for is checked after the build, by
// scripts/check-tables.mjs, not here.
//
// A self-count was tried and was worthless: every path that saw a table also
// wrapped it, so the two counters could never disagree. And the failure actually
// worth catching — Starlight taking over the markdown config so this never runs
// — leaves both counters at zero. A plugin cannot detect that it was not
// invoked; only something reading the output can.
export function rehypeTableWrap() {
  return (tree) => {
    visit(tree, (node, index, parent) => {
      if (!parent || node.tagName !== "table" || index === null) return;
      if (parent.type === "element" && parent.properties?.className?.includes?.("table-wrap")) {
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
    });
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
