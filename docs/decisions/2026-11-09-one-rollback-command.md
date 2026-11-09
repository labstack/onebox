# One command goes back

- Status: accepted and implemented
- Date: 2026-11-09
- Scope: CLI recovery commands and `ob deploy` flags; the authored Application
  `onebox.run/v1alpha2` and host state are unchanged

## Decision

Fold `ob abort` into `ob rollback`. Recovery is two verbs: `ob resume` goes
forward, `ob rollback` goes back. `ob rollback` reverts an interrupted deploy
when the journal records one (the former abort, under the same migration gate,
with `--break-migration-gate` and `--break-lock`), and otherwise activates the
previous release. It goes back one step per invocation, and it refuses when the
journal cannot be read.

A journal cannot tell an interrupted deploy from one still running, and the
lock protocol lets a recovery of a deploy reclaim that deploy's own lock. So
the revert refuses with `deploy_in_progress` while the deploy lock is held by
that deploy and was refreshed within three heartbeats (`3 × lockTTL / 10`);
`--break-lock` overrides it.

Remove `ob deploy --yes`. Under the default `requireApproval: true` it skipped
the prompt and the engine then refused for want of a confirmation; with
approval off it was an unattended deploy. The unattended path is now
`ob plan` then `ob deploy --plan ob-plan.json`, which applies a non-critical
plan without a prompt when approval is off.

Both removals answer the old spelling with its replacement instead of an
unknown-command or unknown-flag error. Structured results carry a `rollback`
field (`interrupted_deploy` or `previous_release`) so a consumer need not infer
which happened.

## Why no API version change

The authored API identity versions the project file. Neither change alters what
`ob.yml` accepts or how a host is laid out, so `onebox.run/v1alpha2` stands. A
change to the accepted project file — such as removing a service driver —
advances it, per [the v1alpha1 baseline](2026-09-20-application-v1alpha1.md),
and recorded releases must stay loadable by recovery commands.

## Migration

[Upgrade to one rollback command](../../site/src/content/docs/docs/guides/upgrade-cli-recovery.mdx).
