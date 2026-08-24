## What this changes

<!-- What does this pull request do, and why? Link the issue it settles. -->

Closes #

## Why this is correct

<!--
Onebox's value is that its output can be trusted without reading its source, so
a change that is merely plausible is not enough. Explain how you know this
behaves the way the description says: the test that fails without it, the
output you compared, or the invariant it preserves.
-->

## Effect on the safety envelope

<!--
Does this change what Onebox will do to a running production system, or move
something between "the schema accepts it" and "the binary does it"? If so, say
so here and note the corresponding /status/capabilities update. Write "None" if
neither applies.
-->

## Checklist

- [ ] `just check` passes locally.
- [ ] Tests cover the new behaviour, including the failure paths.
- [ ] Generated documentation is current (`just check` verifies this).
- [ ] I have accepted the [CLA](https://github.com/labstack/onebox/blob/main/CLA.md), or will when the bot asks on my first pull request.
