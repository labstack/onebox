# Contributing to Onebox

Onebox is a deployment tool that holds a production application on someone
else's server. A change that is merely plausible is not good enough: the
project's value is that its output can be trusted without reading its source.
That standard shapes everything below.

## Licensing and the CLA

Onebox is licensed under the [Apache License, Version 2.0](LICENSE).

Before your first pull request can be merged, you must accept the
[Contributor License Agreement](CLA.md). You keep the copyright in your work;
the CLA grants LabStack LLC the rights needed to distribute it, including under
future commercial licensing. An automated check will prompt you on your first
pull request, and will not ask again afterward.

If you cannot accept the CLA, you can still file issues, reproduce bugs, and
review pull requests. Those are genuinely useful and carry no agreement.

## Before you write code

For anything beyond a typo or an obvious bug fix, **open an issue first.**
Onebox has an explicit, narrow scope — one application, one active production
host — and a documented safety envelope. A change that widens either is a
product decision, not a code review question, and it is far cheaper to settle
before the code exists.

[`docs/product.md`](docs/product.md) records the direction, and
`/status/capabilities` records what the binary does today versus what the schema
merely accepts. A change that moves something between those two states should
say so in its description.

## Setting up

You need Go (the version in [`go.mod`](go.mod)),
[`just`](https://github.com/casey/just), Node.js, and Docker for the
end-to-end suite.

```sh
just site-install   # once, on a fresh clone: installs site/node_modules
just build          # builds ob into $OB_BIN_DIR, or ~/.local/bin
```

## The verification gate

Run this before every commit:

```sh
just check
```

`check` formats, vets, tests, verifies the generated documentation is current,
and builds the site. It contacts no target host and writes nothing to your
tree.

CI runs a superset:

```sh
just ci             # check + lint + govulncheck + actionlint
```

`ci` needs tools the repository does not vendor. That separation is
deliberate — a contributor without them should still be able to run `just
check` and get a truthful answer about their change.

The Docker end-to-end suite is opt-in and slower:

```sh
just e2e
```

## Generated documentation

The field reference, CLI reference, and error code pages under
`site/src/content/docs/reference/` are **generated from the binary** by
`cmd/ob-docgen`. Do not hand-edit them; they are committed so that a reader of
the repository sees the same reference a reader of the website does.

If your change touches a flag, a field, or an error code, regenerate:

```sh
just docs-generate
```

Then commit the result. `just check` fails if the committed pages do not match
what the binary produces, and CI fails on any uncommitted change after the
gate runs.

## Tests

New behavior needs a test that fails without the change. Onebox's test suite is
the argument that its safety envelope holds, so the bar is higher than usual
for anything touching:

- plan construction, digests, or approval binding
- SSH transport, command construction, or quoting
- secrets handling
- rollback, activation, or finalization paths

For those areas, cover the failure mode as well as the success path. A
deployment tool is judged by what it does when something goes wrong.

## Commits and pull requests

- Write commit subjects in the imperative mood, prefixed with a type:
  `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`, `style:`.
- Explain *why* in the body. The diff already shows what.
- Keep a pull request to one concern. Two concerns are two pull requests.
- State in the description what you ran, and what you did not.

## Reporting security issues

Do not open a public issue for a security vulnerability. See
[SECURITY.md](SECURITY.md).

## Code of conduct

Be straightforward and civil. Critique the change, not the person. Maintainers
may remove comments, close issues, and block accounts that make the project
worse to participate in.
