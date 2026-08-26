# Documentation map

User documentation lives in [`site/`](../site) and is published as the
documentation website. This directory holds only the artifacts that belong to
the repository rather than to a reader.

| Path | What it is |
|---|---|
| [`onebox.run-v1.schema.json`](onebox.run-v1.schema.json) | The published JSON Schema for the project file. Generated from the Go model by `ob schema` and tested byte-for-byte against it. `app.SchemaID` points at this path on `main`, and `ob init` writes that URL onto the first line of every scaffolded project. |
| [`product.md`](product.md) | Product direction. Not an implementation contract, and not a capability list. |

## Where the user documentation went

| Was | Now |
|---|---|
| `docs/cli.md` | `/reference/cli` — generated from the binary by `cmd/ob-docgen` |
| `docs/schema-v1.md` | `/reference/project-file` and the generated field pages under `/reference/fields/` |
| The authority map that used to live here | `/status/capabilities` — every capability marked shipped, schema-only, or intent-only |

## Which documents are authoritative

Documentation says what is true **today**. Direction lives in
[`product.md`](product.md), and it is not presented as a capability. What the
binary actually does is the generated reference under
`site/src/content/docs/reference/`, produced by `cmd/ob-docgen` and checked
against the binary by `just docs-generate-check`, so it cannot describe
something the loader does not accept.

The site marks three states. The vocabulary is closed — the content schema
rejects a fourth at build time, and the generator's registry is checked against
the same list — but the marking itself is authored, so it is a claim a reviewer
should check rather than one the build can prove:

- **Shipped** — the current binary does this.
- **Schema only** — the loader validates it and it is published in the JSON
  Schema, but the behaviour is an open proposal.
- **Intent only** — validated and carried into plans, but nothing continuous
  runs for it.

## Regenerating

```sh
just docs-generate        # rewrite every generated reference page
just docs-generate-check  # fail when one is behind the binary
just site                 # serve the site locally
just site-build           # build it into site/dist
```

The generated pages are committed, so a reader of this repository sees the same
reference a reader of the site does, and so the check has a baseline to compare
against. They carry a generated marker; edit the generator, not the page.

For service drivers, the name, image repository, port, data path, URL scheme,
health-check availability, and derived connection parts come from a sorted,
read-only projection of the private runtime catalogue. Typical-use labels and
operational limitations remain authored in `cmd/ob-docgen`: they explain the
product rather than restating data the catalogue can prove.
