# yeet

> Working codename — the release gets a searchable, non-slang name.

**Zero-downtime deploys for docker-compose apps: agentless, plan-before-apply, keep your proxy.**

An agentless, single-binary deploy engine for apps described by a docker-compose file: preview every
deploy as a rendered diff and an exact command list, then release with health-gated zero downtime —
keeping your own proxy, database, and conventions.

## Status

**Design phase.** The full design — architecture, service model, deploy lifecycle, state/locking
protocol, rollback semantics, configuration, prior art, risks, and roadmap — lives in
[`docs/design.html`](docs/design.html) (rev 5).

The design has been through two adversarial review rounds (five independent reviewers: architecture,
market/prior-art, operations, fresh-eyes holistic, fix-verification), a generality audit, and a
rev 5 generality & robustness hardening pass (traffic-shift protocol, compose canonicalization,
multi-service roles, resource-exclusivity rule, one mutation regime). The review changelog is §13
of the design doc.

## The wedge

Two capabilities no tool in the single-server deploy niche (Kamal 2, Sidekick, Haloy, Uncloud,
Dokploy, Coolify) offers together:

1. **Plan before apply** — rendered artifacts, pinned images, and the exact remote command list,
   previewed before a single host is touched, with the plan bound to the apply.
2. **Zero-downtime for docker-compose apps as they are** — the compose file is the contract;
   scale–health–drain releases; no proprietary app model, no replacing your proxy.

## Roadmap

- **M0** — walking skeleton: deploy [monk](../monk) end-to-end, zero-downtime, from the new binary
- **M1** — plan/apply + YAML config (embedded CUE validation); retire monk's `yeet.sh`
- **M2** — trustworthy: journal, fencing, resume/abort, migration gate, per-role verify
- **M3** — second app + multi-host executor
- **M3.5** — out-of-distribution validator app (generality proven, not assumed)
- **M4** — open-source release

See §12 of the design doc for exit criteria.
