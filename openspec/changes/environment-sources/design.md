# Design

## What the model has to answer

Every question of the form "does this container get that value" — for any
combination of role, source, scope and environment. The previous rules answered
four such questions in four places and could not answer a fifth, which is how
seven distinct behaviours arrived without anyone deciding on them.

The test of the model is not that it handles `job`. It is that it answers cases
nobody has raised yet without being extended.

## The corner cases, and how one model settles each

| Case | Before | Under the model |
|---|---|---|
| `job` gets project env | no, unstated | yes — it runs the user's code |
| `daemon` gets project env | no, unstated | no — it is a server you author |
| `daemon` that needs a credential | impossible | names the source |
| `compose:`-sourced application | silently nothing | same as any application; source is not a factor |
| several sources declared | alphabetically first | refused unless something selects |
| per-environment values | impossible | the environment selects |
| withholding from one workload | impossible | the empty list |
| plaintext vs encrypted | different rules | one rule; kind is a property of the source |
| ordering between them | undefined | declared order, later wins |
| a generated credential vs a declared one | undefined | the connection wins, always |

Ten answers, four rules. That ratio is the point; the previous ratio was four
answers, four rules, and the fifth question had no answer at all.

## Why "source" rather than "env file" and "secret"

They differ in how bytes are obtained and in nothing else. Both contribute
environment values to a container. Both want per-workload control, ordering,
and an environment-specific selection. Modelling them separately meant each
capability had to be built twice, and in practice the second one was not: env
files got per-workload lists, secrets got nothing.

Encryption is a storage property. A plaintext file on a laptop is not less
sensitive than a SOPS file — it is less protected. The contract should not
treat "how it is stored" as "who may have it", and the redaction requirement
applies to both for exactly this reason.

## Why selection is explicit

The old behaviour picked the alphabetically first source. It is worth being
precise about why that is worse than picking the first declared, or the one
matching the environment's name:

Both alternatives are guesses that are right often enough to be trusted and
wrong silently. A project with `production` and `staging` sources deploying to
staging would get production's under alphabetical order, and would get the
right one under name-matching — until someone names a source `shared` or
`eu-west`, and then name-matching quietly produces nothing or the wrong thing.

A guess that is usually right is the worst kind, because it is trusted. Refusing
an ambiguous selection costs one line in a project file and removes an entire
class of incident.

## Why source must not affect what a workload receives

This is the clause most likely to be eroded, so the reason is recorded rather
than assumed.

`compose:` exists so a container someone already wrote can be adopted without
rewriting it. The overlay applies Onebox's identity and routing to it. It is
tempting to treat the referenced file as complete — the author wrote it, so
leave it alone — and that is how the environment came to be dropped. But the
author also declared it an `application` in the project, and an application
receives the project's environment. Those two statements are about different
things, and the second is not overridden by the first.

The general form: the project file states intent, the referenced file states
implementation, and intent decides what reaches the container.

## What this deliberately does not do

**No per-key scoping or filtering.** The unit is the source. A project needing
finer separation writes more sources. Key-level rules would need their own
precedence model, and precedence models are what this change exists to
collapse.

**No new secret providers, no change to decryption, staging, or push.** Only
who receives, in what order.

**No inheritance between sources.** A source does not extend another. Ordering
composes them; that is enough and is already understood from environment files.
