# One model for the environment a container receives

## Why

Four functions decide what environment reaches a container, and no document
states the model they are implementing, because there is not one. Probing the
boundaries found seven behaviours, none of them written down and several of
them wrong:

1. **Role decides, and the roles are unstated.** A project-wide environment
   file reaches `application` and `worker`. A `job` receives none — the role
   most likely to need a credential, because backups and retention sweeps are
   jobs.
2. **Source silently overrides role.** A workload sourced from `compose:`
   receives no environment file at all, whatever its role. An `application`
   gets the project's environment; the same application, adopted verbatim from
   a Compose file, gets nothing. Nothing says so.
3. **Secrets have no stated rule.** Which workloads receive decrypted values
   exists only in code, mirroring the env-file roles. Nothing recorded whether
   that was a decision.
4. **Multiple secrets resolve arbitrarily.** A project declaring more than one
   SOPS secret gets whichever name sorts first. Not the first declared —
   alphabetically first.
5. **The environment is never consulted.** `secrets: {production: …, staging: …}`
   reads like a per-environment declaration and is not one. Deploying to
   staging ships production's secrets, silently, because "production" sorts
   before "staging".
6. **Neither can be overridden per environment.** `env_files` and `secrets` are
   not in the overridable set, so there is no legitimate way to vary either —
   which is why (5) is not merely a bug but a dead end.
7. **Declining is inexpressible.** An empty list reads as absent, so it takes
   the project default. There is no way to say "this workload receives
   nothing".

(5) is the serious one. A project that writes the obvious thing gets production
credentials in staging with no error, no warning, and nothing in the canonical
form that differs between the two environments.

These are not seven bugs. They are one absence: no model, so each function
answers for itself and the answers do not compose.

## What changes

**One concept.** A project declares named **environment sources**. A source is
a file contributing environment values. Whether it is plaintext or
SOPS-encrypted is a property of the source, not of the workload receiving it —
today these are two mechanisms with different rules for the same job.

**One resolution rule.** Every workload resolves an ordered list of sources.
The most specific declaration wins: the workload's own list, otherwise the
environment's, otherwise the project's. An explicitly empty list means none.

**One composition order.** Declared sources in order, later overriding earlier;
then managed-service connections, which nothing authored may shadow.

**One default rule, stated as a principle.** Workloads that run the user's code
receive the default list; a `daemon` does not, because it is a server the user
authors rather than code they wrote. **A workload's source never changes what
it receives** — only its role does. That clause exists to kill (2), which is
the kind of behaviour that returns the moment it is not written down.

## Impact

- Selecting a secret by name becomes required where a project declares more
  than one. A project declaring exactly one is unaffected.
- `job` and `compose:`-sourced workloads begin receiving the environment their
  role implies. Generated runtimes and frozen verdicts move, deliberately.
- Environments and workloads gain control they did not have; no existing field
  is removed.
