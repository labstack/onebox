# Security policy

Onebox holds SSH access to production servers and handles application secrets.
Security reports are taken seriously and are welcome.

## Reporting a vulnerability

**Do not open a public issue.**

Report privately through either channel:

- [GitHub private vulnerability reporting](https://github.com/labstack/onebox/security/advisories/new)
  — preferred, keeps the report and the fix in one place.
- Email <security@labstack.com>.

Please include what you can: affected version (`ob version`), a description of
the issue, the impact you believe it has, and steps or a project file that
reproduce it. A partial report is better than no report; do not wait until you
have a full exploit.

We will acknowledge your report within 3 business days and give you an
assessment and expected timeline within 10 business days. You will be kept
informed as the fix progresses, and credited in the advisory unless you prefer
otherwise.

Please give us a reasonable opportunity to release a fix before disclosing
publicly. We will not pursue legal action against anyone acting in good faith
under this policy.

There is no bug bounty program.

## Scope

Onebox's threat model assumes the operator's workstation and the target host
are trusted, and that SSH itself is sound. In scope:

- Remote command construction, quoting, and injection through project file
  values, image references, or host responses.
- Secrets appearing in logs, plan artifacts, generated Compose files, process
  arguments, or the target host's filesystem beyond their intended location.
- Approval or plan digest binding that can be forged, replayed, or made to
  apply to a plan other than the one approved.
- Privilege escalation on the target host beyond what the operator's SSH user
  already holds.
- Any path where Onebox reports success for a change that did not happen, or
  reports a safe state that is not the actual state.

Out of scope:

- Vulnerabilities in Docker, Compose, the operating system, or the
  application being deployed.
- An operator with a valid SSH key doing damage they were already authorized
  to do.
- Anything requiring a compromised workstation or a malicious operator.
- Denial of service against the operator's own host.

## Supported versions

Onebox is pre-1.0. Fixes land on the latest release only. There is no
backporting to earlier versions and no long-term support branch.
