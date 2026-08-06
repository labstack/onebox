# One mechanism for the environment a container receives

## Why

Four code paths decide what environment values reach a container, and no
document states the model they implement, because there is not one. Probing the
boundaries found seven behaviours, none written down and several wrong:

1. `runtime.env_files` reaches `application` and `worker`. A `job` receives
   nothing — the role most likely to need a credential, since backups and
   retention sweeps are jobs.
2. A `compose:`-sourced workload receives no environment file at all, whatever
   its role. The same application declared inline receives the project's.
3. Which workloads receive decrypted secrets exists only in code.
4. A project declaring several SOPS secrets gets whichever name sorts first.
5. The environment is never consulted, so `secrets: {production: …, staging: …}`
   reads like a per-environment declaration, is not one, and ships production's
   credentials to staging silently.
6. Neither field can be overridden per environment, so there is no correct way
   to do it.
7. An empty list reads as absent, so "receives nothing" is inexpressible.

(5) is the serious one: the obvious authoring produces cross-environment
credential leakage with no error and nothing in the canonical form that differs.

## What changes

**No new field and no new concept.** `env_files` is generalised: an entry is a
path, `{file: …}`, or `{file: …, provider: sops}`. Encryption becomes a property
of the entry rather than a separate mechanism with its own rules. Adding an
object form to a list of scalars is the additive move the contract already
blesses.

An earlier draft of this change invented a `sources` concept with a named
registry. It was rejected on review: "source" already means a workload's
`build`/`image`/`compose` in both binding contracts, so a workload with a
`sources` field would carry two meanings of the word — and the registry's only
purpose was to give overrides something to name, which wholesale list
replacement makes unnecessary.

**`secrets` is withdrawn** with a directed error naming the entry form that
replaces it. Reinterpreting its keys as environment names was considered and
rejected: they are arbitrary names today, so the reinterpretation would silently
change a project's meaning in exactly the way (5) does.

**Three scopes, one field**: `runtime.env_files`,
`environments.<name>.env_files`, `workloads.<name>.env_files`, plus a workload
override. Most specific wins; an explicit empty list means none.

**The default follows the role, never the source.** `application`, `worker` and
`job` receive it; `daemon` does not, and may be given a list explicitly.

**Precedence is the container runtime's**, stated in full, with refusal — not
ordering — protecting generated credentials from being shadowed at either layer
that can shadow them.

## Impact

- `secrets` stops loading. No third-party project in the corpus declares it.
- `env_files: []` changes meaning from "absent" to "none"; jobs and
  compose-sourced workloads begin receiving what their role implies. Verdicts
  and generated runtimes move, enumerated when the corpus is re-frozen.
- The growth requirement gains an explicit pre-release gate that expires at the
  first published release, so this is a stated exception rather than a quiet
  one.
- `runtime-generation`'s overlay gains a fourth row and an ejection rule; the
  enumeration grows rather than weakening.
