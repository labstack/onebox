# Design

## How this design was reached, and what it replaced

The first draft invented a `sources` concept: a named registry of environment
sources, referenced by name from each scope. Two independent reviews and a
design pass rejected it, and the reasons are worth keeping because they are the
reasons this shape is right.

**The name collided.** "Source" already means a workload's `build`, `image` or
`compose` in both binding contracts. The draft's own sentence — *a workload's
source SHALL NOT affect what it receives* — becomes self-contradictory the
moment that workload also carries a field called `sources`.

**The registry served nothing.** Names existed so an override could refer to an
entry. But every list in this contract replaces wholesale, so an override writes
the full list and never refers to an entry. Removing names dissolved an entire
open question about what names the existing `env_files` shorthand would produce.

**The generalisation was already available.** An encrypted file is a file. Adding
an object form to a list whose entries were scalars is exactly what the growth
requirement blesses, and every existing project keeps its meaning.

The result is a smaller change than the draft where it counts — no new field, no
new noun, `secrets` removed rather than given a parallel shorthand — and a
larger one where the draft was silent: the overlay row, the ejection strip, the
referenced-`environment` refusal, decrypted staging, and the growth-rule
amendment written out instead of left as three options.

## The corner cases, and the rule that settles each

This table is normative for behaviour; `tasks.md` requires a test per row.

| Case | Before | Under the model |
|---|---|---|
| `job` gets the project's environment | no, unstated | yes — the role gate admits it |
| `daemon` gets it | no, unstated | no — application configuration does not reach infrastructure by default |
| `daemon` needing a credential | impossible | declares its own list |
| `compose:`-sourced application | silently nothing | same as any application; source is not a factor |
| several entries declared | alphabetically first | all apply, in declared order |
| per-environment values | impossible | the environment declares a list, or overrides a workload's |
| withholding from one workload | impossible | the explicit empty list |
| plaintext vs encrypted | different mechanisms | one field; kind is a property of the entry |
| ordering between them | undefined | declared order, later wins |
| a connection vs an authored value | undefined | connection wins; a claim on its name is refused |
| an authored endpoint with a generated credential | inexpressible | map the credential parts, author the host |
| a referenced service's own `environment` | undefined | outranks projected entries, except on a connection variable, which is refused |

## Why refusal rather than ordering, for credentials

The container runtime places `environment` above `env_file`. Onebox delivers
resolved entries and connection files as `env_file`. So any `environment` key —
a workload's inline `env`, or a referenced service's own — outranks a
connection, and no ordering Onebox chooses can change that. A generated runtime
cannot contradict the runtime that reads it.

The earlier draft asserted connections "cannot be shadowed" and was simply
wrong. The claim is made true one level up: a key claiming a connection
variable fails validation. A rule the layer below cannot enforce is enforced by
the layer above.

Non-connection keys are left alone deliberately. A referenced service's
`environment` is the author's most specific statement about that container, and
refusing there would make adopting Immich's or authentik's published compose
impossible without editing them — which defeats what `compose:` is for.

## Why the daemon default is opt-in, and what it costs

The cost is real: the common upstream layout shares one file across every
service, so converting authentik means naming the file on its Postgres too. The
existing corpus conversion already does exactly that, so the cost is one the
real conversion already pays.

It is accepted because the two failure directions are not symmetric. A default
wrong in the safe direction fails at container startup naming the missing
variable, and is fixed in a minute. A default wrong in the other direction puts
the application's third-party credentials inside its database, and is never
observed at all.

The earlier justification — "a daemon is a server you author, not code you
wrote" — was falsified against the corpus: in every third-party conversion, no
container is code its author wrote, and Immich's machine-learning image appears
as a `daemon` in one conversion and a `worker` in another, the deciding factor
being whether it should receive the environment. A principle chosen by the
outcome it is meant to decide cannot settle the next case, so the reason is now
about what the configuration is rather than who wrote the container.

## What the corpus is evidence of

The corpus holds third-party conversions and projects belonging to this
repository's author. Only the former say anything about applications in general.

From the third-party set alone: authentik shares one `.env` across an
application, a worker and a Postgres daemon; Immich shares one across an
application and a machine-learning daemon; Paperless gives its file to the
webserver only. None declares a project-wide list or a `secrets` block — which
is also why the meaning changes here have a measured footprint of nearly zero,
and why the pre-release gate is honest rather than convenient.

Composition order rests on the container runtime's own `env_file` semantics
rather than on a corpus observation, since no third-party conversion declares
more than one file. The runtime is the better authority for it regardless: a
project file that meant something different by a list than the runtime it
generates would be its own defect.

## What this deliberately does not do

- **No named registry, no reference-by-name.** Wholesale replacement makes names
  unnecessary; adding them later is additive if a need appears.
- **No per-key scoping or filtering.** The unit is the file. Finer separation
  means more files.
- **No merge operators for overrides.** One merge semantic already governs every
  list in the contract; a second is a precedence model, which is what this
  change exists to collapse.
- **No entry selection by name matching.** A guess that is usually right is the
  worst kind, because it is trusted.
- **No new providers and no change to decryption or push.** Only who receives,
  in what order, declared where, and where the decrypted bytes live.
- **Build-time values** a local build hook reads are not container environment.
  Named in the authoring guide, not modelled here.
