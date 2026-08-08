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
  return (tree) => {
    visit(tree, (node, index, parent) => {
      if (!parent || node.tagName !== "table" || index === null) return;
      if (parent.type === "element" && parent.properties?.className?.includes?.("table-wrap")) {
        return;
      }
      parent.children[index] = {
        type: "element",
        tagName: "div",
        properties: { className: ["table-wrap"], tabIndex: 0, role: "region" },
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
