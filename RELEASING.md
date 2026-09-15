# Releasing craze

The general release framework is `cc-plugins:release-workflows`; this file
documents what's specific to this repo.

## TL;DR

    /release-workflows:release v0.0.1

That's it. Everything else is automatic.

## What happens

1. **`release-workflows:release`** (LLM, local):
   - Verifies branch (`main`) + clean tree + `ci-success` green on HEAD
   - Asks/confirms version
   - Drafts a CHANGELOG entry from `git log v<previous>..HEAD`, commits as
     `docs(changelog): vX.Y.Z entry`
   - Runs `scripts/release/update-version.sh X.Y.Z` → bumps
     `internal/version/version.go`
   - Commits as `chore(version): bump to X.Y.Z`
   - Tags `vX.Y.Z` (annotated) on the version commit
   - `git push --follow-tags` (admin bypasses the ruleset)

2. **`release.yml`** (CI, on tag push `v*`):
   - **version-check** — asserts the tag matches
     `internal/version/version.go`
   - **ci-gate** — polls `ci-success` on the tagged commit (45-minute
     budget)
   - **release** (`needs: [version-check, ci-gate]`):
     - Checks out, sets up Go
     - Extracts this tag's `CHANGELOG.md` section via
       `scripts/release/changelog-section.sh` for GoReleaser's release
       notes (falls back to GoReleaser's auto-generated notes if the
       section is missing)
     - Mints a release-bot App token scoped to `charliek/homebrew-tap`
     - Runs `goreleaser release --clean`, which:
       - Builds 4 targets (`linux`/`darwin` × `amd64`/`arm64`) with the
         version injected via ldflag
       - Tarballs them as `craze_<os>_<arch>.tar.gz`
       - Builds `.deb`s (amd64 + arm64) via `nfpms:`
       - Creates the GitHub Release (non-draft — `.goreleaser.yaml` sets
         `release.draft: false`) and uploads the tarballs, `.deb`s, and
         `checksums.txt`
       - Pushes `Formula/craze.rb` to `charliek/homebrew-tap` (the
         `brews:` step, using the App-minted token; `skip_upload: auto`
         keeps prerelease tags out of the tap)
     - On a non-prerelease tag only: mints a release-bot App token scoped
       to `charliek/apt-charliek` and dispatches its `publish` event
       (`event_type=publish`, 3 retries with backoff)

The maintainer runs step 1; everything else is automated.

## Version files this repo owns

`scripts/release/update-version.sh` bumps:

- `internal/version/version.go` — the `var Version = "X.Y.Z"` line. This
  is the only source-tree version manifest craze has; the Go binary has
  no other literal version string, and the built binary's `--version`
  output always comes from an ldflag override (`make build` → `dev`,
  GoReleaser → the tag) rather than from this default.

NOT bumped:

- `pyproject.toml` — the Zensical docs toolchain's own version, tracked
  independently of craze releases.
- `tests/cli/pyproject.toml` — the pytest CLI harness's manifest; it
  tracks the test tooling, not the shipped binary.

## Snapshot / dev versioning

Not used. Main between releases shows the last released version in
`internal/version/version.go`. `make build` always overrides that to
`dev` via ldflags. CI's `release-snapshot` job (in `ci.yml`) additionally
proves GoReleaser's own snapshot build reports a `SNAPSHOT` version on
every push — neither path touches the source tree.

## Secrets

| Secret | Purpose | Required? |
|---|---|---|
| `RELEASE_BOT_CLIENT_ID` | release-bot GitHub App Client ID | required |
| `RELEASE_BOT_APP_KEY` | App private key (`.pem`) | required |

Both are minted at workflow time into short-lived, repo-scoped
installation tokens — once for `homebrew-tap` (GoReleaser's `brews`
push), and, on non-prerelease tags only, once for `apt-charliek` (the
publish dispatch).

## Branch protection

`main` is protected by a ruleset named `main-protection` (id `23485423`)
with `required_status_checks=['ci-success']` and two bypass actors:

- The release-bot App (App ID `3902108`, type `Integration`) — not
  currently exercised by any post-build push back to `main` in this
  repo, but listed in case a future job needs one
- Admin role (id `5`, type `RepositoryRole`) — lets
  `/release-workflows:release` push the changelog + version commits + tag
  without waiting for `ci-success` on commits that haven't been built yet

Inspect or edit at https://github.com/charliek/craze/rules.

## When things break

| Symptom | Cause | Fix |
|---|---|---|
| `ci-gate` times out after 45 minutes | A runner is stuck/slow, or `ci-success` never reported (e.g. a leg crashed before posting its check) | Check the tagged commit's Actions run; once `ci-success` is green there, re-run the failed `release.yml` run |
| `version-check` fails | Tagged a commit whose `internal/version/version.go` doesn't match the tag | Re-bump locally with `scripts/release/update-version.sh <ver>`, commit, push to `main`, cut a fresh patch tag — don't force-update the failed tag |
| GoReleaser fails at `brews` with `Bad credentials` | `RELEASE_BOT_CLIENT_ID`/`RELEASE_BOT_APP_KEY` unset, or the App isn't installed on `homebrew-tap` | Confirm via `sanity-check-app.yml`'s homebrew-tap block; install the App on the tap if missing |
| `Trigger apt-charliek publish` fails after 3 attempts | App not installed on `apt-charliek`, or rate-limited | Confirm via `sanity-check-app.yml`'s apt-charliek block; `apt-charliek` self-heals on its next scheduled scan regardless |
| `Run GoReleaser` aborts mid-upload (transient GitHub outage) | GitHub API/upload flakiness during the publish phase — the Release may be left partially uploaded, and the homebrew/apt steps that follow it in the job never ran | Re-run the failed workflow run once GitHub recovers — `mode: replace` in `.goreleaser.yaml` reuses the existing Release and retries only what's missing |
| Formula push succeeded but `brew install` finds the old version | Homebrew tap cache | `brew untap charliek/tap && brew tap charliek/tap` |
| GoReleaser logs a `brews` deprecation warning | Expected. GoReleaser recommends `homebrew_casks` over `brews`, but casks are macOS-only and this tap needs to install on Linux too, so `brews` is kept on purpose (see the comment in `.goreleaser.yaml`) | No action |
| A macOS runner outage blocks every merge to `main` | `ci-success` (`ci.yml`) now includes a macOS leg on the `test`/`cli` matrices plus a GoReleaser snapshot build (`release-snapshot`), so `ci-success` can't go green while `macos-latest` runners are unavailable | Wait out the outage; don't disable the macOS leg to work around a transient one |
| Rerunning one red matrix leg leaves `ci-success` green while the other leg is still failing | GitHub Actions quirk: an aggregator job's rerun can see a matrix job as "successful" even when only one of its legs was rerun (github.com/orgs/community/discussions/26822) — see the comment on the `ci-success` job in `ci.yml` | Always rerun **all** jobs in the run, never a single matrix leg |
| `ci-gate` passes against a check nobody expected | The poll matches on SHA and check name only — not workflow, event, branch or app. A completed `ci-success` already attached to that SHA satisfies the gate. In practice the tagged SHA is a fresh `main` commit whose only `ci-success` is its own, so this is accepted rather than fixed (the upstream convention's job is used verbatim) | If a release ever looks like it gated on the wrong run, check the tagged commit's check-runs by hand before trusting it |
| The GitHub Release is public but `brew install` still has no `craze` formula | GoReleaser publishes the Release *before* the `brews` push, so a tap failure leaves a released-but-untapped version — and the apt dispatch never runs either, since it follows GoReleaser in the same job | Fix the tap credentials, then re-run the failed run (`mode: replace` reuses the Release); check the apt dispatch fired afterwards |
| The apt dispatch step is green but no `.deb` appears in the apt repo | A successful dispatch only proves GitHub accepted the event. `apt-charliek`'s own `publish.yml` runs asynchronously and can fail afterwards on its GCP auth, signing key, or smoke test, with nothing reported back here | Watch https://github.com/charliek/apt-charliek/actions for that run; re-dispatch with `gh workflow run publish.yml -R charliek/apt-charliek` once fixed |
| Two tags released close together disagree about `Formula/craze.rb` | `concurrency` is keyed on `release-${{ github.ref }}`, which serialises re-runs of the *same* tag but not two different tags. Both releases push the same formula file, so whichever finishes last wins regardless of version order | Release one tag at a time; if it happens, re-run the newer tag's workflow to push its formula last |

## Break-glass recovery

### Rerun the failed release run

Most `release.yml` failures (transient GitHub API/upload errors, a slow
`ci-gate` poll) are safe to just re-run:

```bash
RUN_ID=$(gh run list -R charliek/craze --workflow release.yml \
                     --limit 1 --json databaseId --jq '.[0].databaseId')
gh run rerun "${RUN_ID}" -R charliek/craze --failed
```

`concurrency: release-${{ github.ref }}` serializes this safely against
the tag, and GoReleaser's `mode: replace` makes the re-run idempotent.

### Homebrew formula push by hand

If GoReleaser uploaded the Release but the `brews` push failed, PUT the
formula directly (needs the current blob `.sha`):

```bash
gh api -X PUT repos/charliek/homebrew-tap/contents/Formula/craze.rb \
  -f message="Brew formula update for craze version vX.Y.Z" \
  -f content="$(base64 -i craze.rb)" \
  -f sha="$(gh api repos/charliek/homebrew-tap/contents/Formula/craze.rb --jq .sha)"
```

Then `brew update && brew upgrade craze`.

### apt-charliek re-dispatch

The dispatch step already retries 3× with backoff. If it still fails, or
you need to force a re-scan without waiting for the next scheduled one:

```bash
gh workflow run publish.yml -R charliek/apt-charliek
```

`apt-charliek`'s `publish.yml` accepts `workflow_dispatch` and re-scans
every tracked package's latest Release, so it picks up the new `.deb`s
idempotently (the payload is ignored).

## Adopting the convention (for new contributors)

If you're new to this repo and need to understand the release pipeline,
read [`cc-plugins/plugins/release-workflows/references/convention.md`](https://github.com/charliek/cc-plugins/blob/main/plugins/release-workflows/references/convention.md)
in the framework repo. It defines the contract every file in this repo's
`scripts/release/` and `.github/workflows/release.yml` is written
against.

## Notes for this repo

- **No `finalize-release` job**: GoReleaser's own `release:` config in
  `.goreleaser.yaml` already creates a non-draft Release
  (`draft: false`), and its `brews:` step hashes the archives it just
  built locally rather than downloading them back from the public
  download URL — so there's no draft-flip or CDN-propagation race to
  guard against here.
- **No test/lint re-run in the `release` job**: `ci-gate` already proves
  the tagged commit's `ci-success` is green, so repeating `go test` /
  golangci-lint in `release.yml` would only re-run work CI already did.
- **No `make build` smoke check**: `internal/version.Version` defaults to
  `dev` with no ldflag override in a bare `make build`, so a smoke step
  here would print `dev` and prove nothing that `version-check` +
  `ci-gate` haven't already proven.
- `cmd/craze-fake-agent` is a test double for the ACP client, not a
  shipped binary — it has no entry in `.goreleaser.yaml`'s `builds:` and
  ships in no release artifact.
- Intel macOS (`darwin/amd64`) is cross-compiled by GoReleaser but not
  exercised on real Intel hardware at runtime — best-effort only. Apple
  Silicon (`darwin/arm64`) is what CI's `macos-latest` runners actually
  test.
- The first release is `v0.0.1`.
- Migrating `brews` → `homebrew_casks` is future work. GoReleaser
  deprecates `brews` in favor of `homebrew_casks`, but casks are
  macOS-only and this tap needs to install on Linux too, so `brews` stays
  on purpose until GoReleaser offers a cross-platform replacement.
