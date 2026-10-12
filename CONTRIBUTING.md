# Contributing to Movian Go

Movian Go preserves Movian's C behavior while using idiomatic Go architecture.
Read [README.md](README.md), the relevant subsystem documentation, and the
existing implementation before changing a ported path.

## Scope and existing contracts

- Keep a change focused on its requested behavior. Do not mix unrelated lint
  fixes, refactors, API removals, or dependency changes into a feature PR.
- If inherited target-branch failures block required checks, identify them and
  obtain explicit scope approval before including fixes. Keep those fixes in a
  separate commit or PR instead of silently expanding the feature.
- Preserve existing extension points and optional mechanisms when they are
  unrelated to the change. An inactive hook is not automatically obsolete;
  do not infer the original author's future plans either.
- When a scoped change makes code obsolete, remove it together with its callers
  and obsolete comments. Do not leave compatibility shims or misleading notes.
- Preserve other contributors' uncommitted work. Separate unrelated changes
  rather than discarding or silently including them.

## Comments in C-to-Go ports

Existing porting comments, including czz's C mappings and architectural notes,
are part of the correspondence between the implementations. Preserve useful
comments and update them to describe the code that actually exists.

- Keep C function names, source-file references, and explanations of ownership,
  locking, initialization order, and behavior compatibility.
- Follow the surrounding comment layout, including existing section headings.
  Keep source comments in English.
- For a changed or newly ported path, identify the C counterpart with `C:` and
  explain significant Go adaptations with `Go:`. Explain why the adaptation is
  needed instead of narrating each statement.
- Verify references against the actual C source. Function name and file path
  are the minimum useful anchor; line ranges are optional and must be checked.
  Record the source repository/revision in the PR when updating line anchors.
  Do not invent line numbers or churn untouched anchors against another version.
- Label behavior absent from the referenced C implementation as `Go extension:`.
  Do not present a new Go feature or additional failure-handling policy as a
  direct C translation.
- Remove or rewrite a stale stub/limitation comment when the corresponding
  behavior is implemented. Remove a comment when its code is removed or the
  explanation is no longer useful, not merely because it is old.

For a ported operation, use the existing function-heading style:

```go
// esFileWrite — C: es_file_write (es_fs.c)
// Go: ToBuffer converts strings as well as binary buffers, preserving
// the updater's JSON-write contract.
```

For functionality added by this port, distinguish the extension explicitly:

```go
// InstallPluginFromArchive installs and loads a ZIP supplied by the caller.
// Go extension: HTTP uploads supply a reader; the caller retains its ownership.
```

Upstream C sources are available in [andoma/movian](https://github.com/andoma/movian).
When a counterpart comes from a different Movian fork, identify that fork rather
than attributing its behavior to upstream.

## Verification

- Use the repository's existing build and verification workflow; see
  [BUILDING.md](BUILDING.md). Run the checks required by CI before pushing.
- Exercise the changed path in the actual application or a focused standalone
  smoke program. An HTTP success response or accepted event alone is not proof:
  inspect the resulting page, installed archive, state transition, or error.
- Check behavior boundaries and failure recovery where the change affects them.
  Reuse existing regression coverage rather than testing source text or wiring.
- `*_test.go` files are local-only by default. When a behavior regression needs
  permanent coverage, add a narrow, path-specific exception in `.gitignore` and
  commit that test with its fix. Do not force-add unrelated local test files.
  Keep generated binaries, diagnostic indexes, and smoke fixtures untracked.
- Report the commands and scenarios actually exercised. State missing toolchain
  prerequisites and untested platforms; do not claim unobserved C parity.

## Commits and documentation

- Base a PR on its intended target branch. Keep commits logically ordered and
  self-contained: shared implementation and affected callers belong together.
- Include the relevant comments and behavior documentation with the change.
  Contribution rules may have their own documentation commit.
- Describe intentional differences from C and public interfaces in the PR.
  Update comments and docs when a behavior or supported contract changes.
