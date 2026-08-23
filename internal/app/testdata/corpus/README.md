# Conformance corpus

Real projects, authored against `onebox.run/v1`, used to freeze the contract:
each one's accept/reject verdict, error code and generated-runtime digest is
recorded in `../contract-verdicts.json` and asserted on every run.

These began life inside the change that introduced the contract. They outlived
it — a corpus is only useful while it keeps being checked, and archiving the
change took the corpus out from under the tests that depend on it. It lives
here now, with the tests.

Adding a project here is deliberate: the harness refuses an unfrozen case, so a
new file must have its verdict recorded with `ONEBOX_UPDATE_VERDICTS=1` and the
diff reviewed.
