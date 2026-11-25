# ADR 0008: Take each upstream minor release within two weeks, by script

Status: Replaced by ADR 0016 (2026-11-25): the gateway code is ours and is edited directly; there is no re-import.
Date: 2026-11-21

## Context

The gateway is Bifrost, renamed down to its internals (Phase 5a). Upstream ships often: 6 to 8 `transports/v*` tags a month in 2026, a new minor version every one to two weeks (`git for-each-ref refs/tags/transports/*`). We are pinned to `transports/v2.6.0`.

The rename touches most files: module paths, imports, identifiers, file and directory names. A normal `git merge` of upstream into our renamed tree would conflict almost everywhere. That is why the brief requires a deterministic, idempotent rename script.

## Options considered

1. **Merge upstream into our tree.** Rejected: every renamed line conflicts.
2. **Regenerate from the pristine tag every upgrade.** Fetch the new tag, run the rename script on it, apply our patch set, run the checks. Conflicts appear only where upstream changed lines that our own patches touch.
3. **Never upgrade.** Rejected: provider APIs change monthly, and security fixes matter.

## Decision

Option 2, with this cadence:

- **Minor releases** (`vX.Y.0`): adopt within two weeks of release, so we are never more than one minor behind.
- **Patch releases**: adopt only for security fixes or bugs that affect us. Security fixes are adopted the same week.
- **Major releases**: planned as their own project.

Mechanics:

- `apps/gateway/UPSTREAM` (the provenance file) records repo, tag and commit. The upgrade procedure reads it.
- Our functional changes live in `apps/gateway/patches/` as an ordered series, applied after the rename. They stay small. Anything large lives in new files (for example our `credits` plugin in its own directory), not in edits to upstream files.
- The rename script and the patches each run in CI on every change: rename the pinned tag from scratch and compare with the committed tree byte for byte.
- An upgrade is one pull request: new tag, regenerated tree, patches reapplied, full checks green.

## Consequences

- Every hand edit to the renamed tree outside the patch series is a defect. CI's reproduction check catches it.
- When upstream refactors code one of our patches touches, the patch must be rebased by hand. Keeping patches small keeps this cheap.
- The rename script needs tests of its own, including the cases a blind replace gets wrong (`maximum`, `maximize`, network module imports, URLs).
