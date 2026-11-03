#!/usr/bin/env bash
# Creates the release tag vX.Y.Z of localrectl and pushes it: the release workflow of GitHub
# Actions then builds and publishes everything (GitHub release, Docker Hub, ghcr.io).
#
# Usage: release.sh [--major | --minor | --patch | --version V] [--pre ID] [--dry-run] [--yes] [--skip-checks]
#   (none)        the version is found from the commits since the last tag:
#                 BREAKING CHANGE or type! -> major, feat -> minor, anything else -> patch
#                 (in 0.x, a breaking change bumps the minor). The first version is 0.1.0.
#   --major, --minor, --patch   force the level of the bump
#   --version V   the exact version, such as 1.2.3 or 1.3.0-rc.1
#   --pre ID      a prerelease of the next version, such as --pre rc: v1.3.0-rc.1, then rc.2…
#   --dry-run     run the checks and print the version, create and push nothing
#   --yes         do not ask for a confirmation
#   --skip-checks skip the local checks (tidy, gofmt, vet, golangci-lint, tests, goreleaser)
set -euo pipefail

die() { echo "release.sh: $*" >&2; exit 1; }
warn() { echo "release.sh: warning: $*" >&2; }
usage() { sed -n '5,14s/^# \{0,1\}//p' "$0" >&2; exit 2; }

BRANCH=master
REMOTE=origin
# Same packages as the patch workflow: internal/httpclient will be replaced by a dependency.
SKIP_PACKAGES='/internal/httpclient$'

level= version= pre= dry=false yes=false checks=true
while [ $# -gt 0 ]; do
	case $1 in
	--major | --minor | --patch)
		[ -z "$level" ] || die "give only one of --major, --minor and --patch"
		level=${1#--} ;;
	--version) [ $# -ge 2 ] || usage; version=$2; shift ;;
	--version=*) version=${1#*=} ;;
	--pre) [ $# -ge 2 ] || usage; pre=$2; shift ;;
	--pre=*) pre=${1#*=} ;;
	--dry-run) dry=true ;;
	--yes) yes=true ;;
	--skip-checks) checks=false ;;
	-h | --help) usage ;;
	*) echo "release.sh: unknown option $1" >&2; usage ;;
	esac
	shift
done
if [ -n "$version" ] && { [ -n "$level" ] || [ -n "$pre" ]; }; then
	die "--version cannot be given with --major, --minor, --patch or --pre"
fi
[ -z "$pre" ] || [[ $pre =~ ^[0-9A-Za-z-]+$ ]] || die "--pre $pre must hold only letters, digits and -"

cd "$(git rev-parse --show-toplevel)"

# The repository: on the release branch, clean, and level with the remote.
current=$(git symbolic-ref --quiet --short HEAD) || die "HEAD is detached: check out $BRANCH"
[ "$current" = "$BRANCH" ] || die "you are on $current: a release is made from $BRANCH"
[ -z "$(git status --porcelain)" ] || die "the working tree is not clean: commit or stash first"
git fetch --quiet --tags "$REMOTE" "$BRANCH"
read -r ahead behind < <(git rev-list --left-right --count "HEAD...$REMOTE/$BRANCH")
[ "$behind" -eq 0 ] || die "$BRANCH is $behind commit(s) behind $REMOTE/$BRANCH: pull first"
[ "$ahead" -eq 0 ] || die "$BRANCH is $ahead commit(s) ahead of $REMOTE/$BRANCH: push first"

# The last stable tag, highest version first; the bump is computed from the commits since it.
last=$(git tag --list 'v[0-9]*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)
if [ -n "$last" ]; then
	range="$last..HEAD"
	[ -n "$(git rev-list "$range")" ] || die "no commit since $last: nothing to release"
else
	range=HEAD
fi

# One line per commit, merges left out: "<subject>" then a tab and "breaking" when its body says so.
commits=$(git log --no-merges --format='%s%x09%b%x1e' "$range" |
	awk 'BEGIN { RS = "\036"; ORS = "\n" } NF {
		sub(/^\n+/, ""); split($0, f, "\t"); s = f[1]
		b = (index($0, "BREAKING CHANGE") || index($0, "BREAKING-CHANGE")) ? "\tbreaking" : ""
		print s b }')

bump=patch untyped=0 nmajor=0 nfeat=0 nfix=0
while IFS= read -r line; do
	[ -n "$line" ] || continue
	subject=${line%%$'\t'*}
	if [[ $line == *$'\t'breaking ]] || [[ $subject =~ ^[a-z]+(\([^\)]*\))?!: ]]; then
		nmajor=$((nmajor + 1)); bump=major
	elif [[ $subject =~ ^feat(\([^\)]*\))?: ]]; then
		nfeat=$((nfeat + 1)); [ "$bump" = major ] || bump=minor
	elif [[ $subject =~ ^(fix|perf|patch)(\([^\)]*\))?: ]]; then
		nfix=$((nfix + 1))
	elif ! [[ $subject =~ ^[a-z]+(\([^\)]*\))?: ]]; then
		untyped=$((untyped + 1))
	fi
done <<<"$commits"
[ "$untyped" -eq 0 ] || warn "$untyped commit(s) without a type (feat:, fix:…) count as a patch"

reason="$nmajor breaking, $nfeat feat, $nfix fix"
if [ -n "$level" ]; then
	bump=$level reason="--$level"
fi

if [ -n "$version" ]; then
	version=${version#v}
	[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "--version $version is not X.Y.Z or X.Y.Z-pre"
	reason="--version"
elif [ -z "$last" ] && [ -z "$level" ]; then
	version=0.1.0 reason="first release"
else
	IFS=. read -r major minor patch <<<"${last:-v0.0.0}"
	major=${major#v}
	# Before 1.0.0, a breaking change bumps the minor (semver: 0.x may change at any time).
	if [ "$bump" = major ] && [ "$major" -eq 0 ] && [ -z "$level" ]; then
		bump=minor
	fi
	case $bump in
	major) version="$((major + 1)).0.0" ;;
	minor) version="$major.$((minor + 1)).0" ;;
	patch) version="$major.$minor.$((patch + 1))" ;;
	esac
fi

if [ -n "$pre" ]; then
	n=$(git tag --list "v$version-$pre.*" | sed -E "s/^v$version-$pre\.//" | grep -E '^[0-9]+$' | sort -n | tail -1 || true)
	version="$version-$pre.$((${n:-0} + 1))"
fi
tag=v$version

git rev-parse --quiet --verify "refs/tags/$tag" >/dev/null && die "the tag $tag already exists"
[ -z "$(git ls-remote --tags "$REMOTE" "refs/tags/$tag")" ] || die "the tag $tag already exists on $REMOTE"

if $checks; then
	for tool in go golangci-lint goreleaser; do
		command -v "$tool" >/dev/null || die "$tool is required by the checks (or use --skip-checks)"
	done
	echo "==> go mod tidy -diff"; go mod tidy -diff
	echo "==> gofmt"
	unformatted=$(gofmt -l .)
	[ -z "$unformatted" ] || die "not gofmt-ed: $unformatted"
	echo "==> go vet"; go vet ./...
	echo "==> golangci-lint"; golangci-lint run
	echo "==> go test"
	# shellcheck disable=SC2046 # one argument per package
	go test -race -count=1 $(go list ./... | grep -Ev "$SKIP_PACKAGES")
	echo "==> goreleaser check"; goreleaser check
fi

log=$(git log --no-merges --format='- %s' "$range")
echo
echo "last version: ${last:-none}"
echo "new version:  $tag ($reason)"
echo "commits:"
echo "$log"
echo

if $dry; then
	echo "dry run: the tag $tag was not created"
	exit 0
fi

if ! $yes; then
	# /dev/tty exists even without a terminal (CI, cron): only opening it tells.
	{ exec 3</dev/tty; } 2>/dev/null || die "no terminal to confirm: use --yes"
	printf 'Create and push %s? [y/N] ' "$tag"
	read -r answer <&3
	exec 3<&-
	case $answer in
	y | Y | yes) ;;
	*) echo "cancelled"; exit 1 ;;
	esac
fi

# A signed tag when git has a signing key (GPG or SSH), else an annotated one.
sign=-a
[ -z "$(git config --get user.signingkey || true)" ] || sign=-s
git tag "$sign" "$tag" -F - <<EOF
localrectl $tag

$log
EOF
if ! git push "$REMOTE" "refs/tags/$tag"; then
	git tag -d "$tag" >/dev/null
	die "the push failed: the local tag $tag was removed, run the script again"
fi

repo=$(git remote get-url "$REMOTE" | sed -E 's#^(git@|ssh://git@|https://)github\.com[:/]##; s#\.git$##')
echo
echo "$tag is pushed. Follow the release at:"
echo "https://github.com/$repo/actions/workflows/release.yml"
