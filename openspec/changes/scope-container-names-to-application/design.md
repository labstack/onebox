## Context

See proposal.md — Why.

`slotNames` (`internal/engine/roll.go:241`) returns `[]string{svc}` for one
replica and `<svc>-1..<svc>-N` beyond that. `roll.go:215` and `roll.go:227` apply
those with `docker rename`. Container names are host-global, so the derivation is
unique only per host per component name — an assumption that holds exactly as
long as one host runs one application.

The rollout already renames containers as part of its normal slot handover, which
is what makes this correction cheap: changing the derived name changes what the
handover renames to, not how it works.

## Goals / Non-Goals

**Goals:**

- Container names unique across applications sharing a host.
- Stable across releases, so rollout, rollback, and observation agree.
- A conflict with a foreign container reported before anything is mutated.

**Non-Goals:**

- No change to Compose project, network, or volume naming. Those are scoped by
  the Compose project already and are addressed by the declarative-schema change.
- No change to rollout, drain, health-gating, or rollback mechanics.
- No migration tooling. The next deployment renames containers, which the
  rollout does anyway.

## Decisions

### Join the application and component with an underscore

The separator must not appear in either identifier, or derivation is ambiguous:
`<app>-<component>` maps both (`a-b`, `c`) and (`a`, `b-c`) to `a-b-c`. The
current identifier grammar (`internal/config/schema.cue`) permits hyphens and
excludes underscore, and the container runtime accepts underscore in container
names, so underscore is the join character.

This matches the derivation the declarative-schema change adopts for every other
generated name, so the two do not diverge.

### Refuse rather than truncate

Docker limits container names, and a truncating scheme with a hash suffix
reintroduces exactly the collision being removed — a review of the schema change
produced a concrete two-input collision under a seven-character suffix. Refusing
at validation is total, costs only unusually long identifier pairs, and relaxing
it later is additive.

### Check for a foreign holder during preflight, not mid-rollout

Today a name conflict surfaces as a failed `docker rename` after new containers
have started, leaving the rollout to unwind. The check is cheap and the
information is available before anything is mutated, so it belongs there.

## Risks / Trade-offs

**Every running installation's containers are renamed on the next deploy.** →
The rollout already renames as part of the handover; release directories,
volumes, networks, and the Compose project are untouched. The observable change
is `docker ps` output. Anything scripted against a bare container name breaks,
which is worth calling out in the release notes.

**Someone may depend on the current names.** → `ob logs` and `ob exec` take
component names and are unaffected. Direct `docker` invocations against
`server-1` are the exposure.

## Migration Plan

1. Ship the derivation with the refusal and the preflight check.
2. Deploy each existing installation once; the handover renames its containers.
3. No coordination required between installations, since each renames its own.

Rollback: reverting the change restores the previous derivation, and the next
deployment renames back. No state outside container names is involved.
