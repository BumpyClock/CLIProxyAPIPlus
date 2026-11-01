# Opt-in local checks

Prepare the Go 1.26+ toolchain from `go.mod` and download pinned modules with
`go mod download`. Enable the tracked pre-push hook explicitly with
`bash scripts/install-git-hooks.sh`. The installer is idempotent, repository-local
and refuses to hide unrelated active hooks or replace another hooksPath. It does
not overwrite hook files. Relative `.githooks` works in linked checkouts; local
Git config is shared by that repository's worktrees.

Push checks require a clean checkout at the pushed commit. Multiple updated refs
must all resolve to HEAD; another commit is rejected instead of checking the
wrong tree. No user state is stashed. Deletion-only ref pushes skip source checks.
New branches, missing remote history and shared/dependency changes broaden
formatting to every tracked Go file. Known ranges inspect both rename sides with
NUL-delimited filenames and check existing changed Go files without rewriting.
Compilation and vet always cover the real project graph. Empty input runs full
checks, rather than claiming that nothing changed.

The hook uses already prepared tools/modules with `GOTOOLCHAIN=local` and
`GOPROXY=off`. It builds `./cmd/server` into disposable temporary output and vets
`./...`, including tests without running them. Missing toolchain/modules fail;
no hook-time download, catalog refresh, provider request, service launch, sync,
tag or publishing occurs. Model catalog identity remains the existing
`MODELS_REPOSITORY_REF` in CI.

`python3 scripts/test-git-hooks.py` runs executable hook/installer fixtures with
stubbed Go tools and no outbound push. Fixture success is not a server or native
runtime test. CI remains responsible for the full application test suite.

Six existing Ubuntu-latest jobs now run on Boat. Docker publishing retains its
existing self-hosted `cliproxy` default and adds a manual `runner=boat` choice.
QEMU/buildx, image names, exact ref checks, trust conditions and permissions are
unchanged. Source changes do not dispatch a release or repair billing. Public
execution is owner-only under the separately configured Boat account policy;
outside contributors and bots remain blocked by that policy.
