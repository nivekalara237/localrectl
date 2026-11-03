# Make a release

A release of `localrectl` is a git tag `vX.Y.Z` pushed to GitHub. `scripts/release.sh` creates it for you; the `release` workflow of GitHub Actions then does the rest.

## What happens

1. `scripts/release.sh` finds the next version, checks the repository and the code, then creates and pushes the tag.
2. On GitHub, the tag starts `.github/workflows/release.yml`:
   1. the `patch` (build, test, coverage) and `security` (trivy) workflows run again;
   2. the Docker image is built, tested and scanned;
   3. after the approval of the `prod` environment, when it asks for one, goreleaser publishes the GitHub release (archives, checksums, SBOM, signatures) and the images on Docker Hub and ghcr.io.

If one step fails, nothing is published.

## Before you start

- You have the right to push tags to `nivekalara237/localrectl`.
- You are on `master`, up to date with `origin/master`, with nothing to commit.
- `go`, `golangci-lint` and `goreleaser` are installed, for the local checks.

## Make the release

See what the script would do first:

```sh
scripts/release.sh --dry-run
```

Then create the release:

```sh
scripts/release.sh
```

The script shows the last version, the new one and the commits it holds, and asks for a confirmation before it creates the tag. At the end, it prints the link to follow the release on GitHub Actions.

## How the version is found

The script reads the commits since the last version, and looks at the start of their message:

- `BREAKING CHANGE` in the message, or a `!` after the type (`feat!:`): a **major** version, `1.4.2` → `2.0.0`
- `feat:`: a **minor** version, `1.4.2` → `1.5.0`
- anything else (`fix:`, `perf:`, `patch:`, `docs:`, `chore:`…): a **patch**, `1.4.2` → `1.4.3`

A type may have a scope: `feat(cli): …`. A commit without a type counts as a patch, and the script warns you about it.

Two special cases:

- **Before `1.0.0`**, a breaking change bumps only the minor: `0.4.0` → `0.5.0`.
- **The first release** is `v0.1.0`.

So, write your commit messages with a type:

```text
feat: add the download command
fix: retry on a 503
feat!: rename --server-url to --url
```

## Options

- `--major`, `--minor`, `--patch`: choose the level yourself
- `--version 1.2.3`: give the exact version
- `--pre rc`: a prerelease of the next version, such as `v0.2.0-rc.1`; run it again for `rc.2`
- `--dry-run`: run the checks and show the version, create nothing
- `--yes`: do not ask for a confirmation
- `--skip-checks`: skip the local checks

A prerelease is published as such on GitHub, and its image gets only its exact tag: never `latest` nor `X.Y`.

## What the script checks

Before it creates anything, the script stops if:

- you are not on `master`
- something is modified or not committed
- `master` is behind or ahead of `origin/master`
- there is no commit since the last version
- the tag already exists, locally or on GitHub

Then it runs the checks of the CI: `go mod tidy -diff`, `gofmt`, `go vet`, `golangci-lint run`, `go test -race` and `goreleaser check`. So an error is found on your machine, not after the tag is pushed.

## The tag

- The tag is annotated, and its message lists the commits.
- It is signed (`git tag -s`) when git has a signing key (`git config user.signingkey`).
- Only the tag is pushed, never a branch. If the push fails, the local tag is removed: run the script again.

## When something goes wrong

- **The release workflow failed before publishing**: fix the problem on `master`, then delete the tag and make the release again:

  ```sh
  git push origin --delete vX.Y.Z
  git tag -d vX.Y.Z
  scripts/release.sh
  ```

- **The release is already published**: never move or reuse its tag, since users may already have downloaded it. Make a new patch release instead.
