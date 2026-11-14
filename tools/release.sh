#!/usr/bin/env bash
# Start a release: pick the next version, fast-forward main to devel, tag it,
# push. The `release` workflow then builds the Docker image, pushes it and deploys.
#
#   tools/release.sh [--promote] [-y] [--no-ci-check] <alpha|beta|stable> [major|minor|patch|X.Y.Z]
#
#   alpha/beta continue the current prerelease line (1.2.0-beta.1 -> beta.2,
#   alpha -> beta.1, -> stable 1.2.0); after a stable release they start the
#   next patch version unless a bump or explicit version is given.
#   --promote tags the current main without fast-forwarding (e.g. turn the
#   tested beta into stable while devel has moved on).
#   The commit's `ci` workflow must have passed (asked from Forgejo; set
#   FORGEJO_TOKEN for a private repo). --no-ci-check skips that, e.g. when
#   Forgejo can't be reached.
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }
rank_of() { case $1 in alpha) echo 0 ;; beta) echo 1 ;; stable) echo 2 ;; esac; }
channel_of() { case $1 in 0) echo alpha ;; 1) echo beta ;; 2) echo stable ;; esac; }
# vX.Y.Z[-alpha.N|-beta.N] -> "X Y Z rank N"
re='^v([0-9]+)\.([0-9]+)\.([0-9]+)(-(alpha|beta)\.([0-9]+))?$'
parse() {
  [[ $1 =~ $re ]] || return 1
  echo "${BASH_REMATCH[1]} ${BASH_REMATCH[2]} ${BASH_REMATCH[3]} $(rank_of "${BASH_REMATCH[5]:-stable}") ${BASH_REMATCH[6]:-0}"
}
ver_gt() { [[ $1 != "$2" && $(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1) == "$1" ]]; }

promote=0 yes=0 ci_check=1
while [[ ${1:-} == -* ]]; do
  case $1 in
    --promote) promote=1 ;;
    -y | --yes) yes=1 ;;
    --no-ci-check) ci_check=0 ;;
    *) die "unknown flag $1" ;;
  esac
  shift
done
channel=${1:-} bump=${2:-}
[[ $channel =~ ^(alpha|beta|stable)$ ]] || die "usage: $0 [--promote] [-y] [--no-ci-check] <alpha|beta|stable> [major|minor|patch|X.Y.Z]"
[[ -z $bump || $bump =~ ^(major|minor|patch|[0-9]+\.[0-9]+\.[0-9]+)$ ]] || die "bump must be major, minor, patch or X.Y.Z"
rank=$(rank_of "$channel")

cd "$(git rev-parse --show-toplevel)"
[[ -z $(git status --porcelain) ]] || die "working tree is dirty"
echo "fetching origin..."
git fetch -q --tags origin

# What gets released.
if ((promote)); then
  git rev-parse -q --verify origin/main >/dev/null || die "origin/main does not exist yet"
  sha=$(git rev-parse origin/main)
else
  [[ $(git rev-parse devel) == $(git rev-parse origin/devel) ]] ||
    die "local devel differs from origin/devel; push (and let CI pass) first"
  sha=$(git rev-parse origin/devel)
  if git rev-parse -q --verify origin/main >/dev/null; then
    git merge-base --is-ancestor origin/main "$sha" || die "main has commits not in devel; can't fast-forward"
  fi
fi

# The commit must have passed CI: a failing one would only start a release
# job that fails the same way (and burn a tag).
if ((ci_check)); then
  remote=$(git remote get-url origin)
  # ssh://git@host/owner/repo.git, git@host:owner/repo.git or https://host/owner/repo.git
  [[ $remote =~ ^(ssh://)?[^@/]+@([^:/]+)(:[0-9]+)?[:/](.+)$ ]] && host=${BASH_REMATCH[2]} path=${BASH_REMATCH[4]} ||
    { [[ $remote =~ ^https?://([^/]+)/(.+)$ ]] && host=${BASH_REMATCH[1]} path=${BASH_REMATCH[2]}; } ||
    die "can't tell the Forgejo server from origin ($remote); use --no-ci-check"
  api="https://$host/api/v1/repos/${path%.git}/commits/$sha/status"
  auth=()
  [[ -n ${FORGEJO_TOKEN:-} ]] && auth=(-H "Authorization: token $FORGEJO_TOKEN")
  status=$(curl -fsS -m 20 "${auth[@]}" "$api") || die "can't read CI status from $host; use --no-ci-check to skip"
  # Only the `ci` workflow: a failed release of this commit doesn't count.
  ci=$(jq -r '[.statuses[]? | select(.context | startswith("ci /")) | .status] | unique | join(" ")' <<<"$status")
  case $ci in
    success) echo "CI passed on $(git log -1 --format=%h "$sha")" ;;
    "") die "no CI result for $(git log -1 --format=%h "$sha") yet; push and wait for the ci workflow" ;;
    *failure* | *error*) die "CI failed on $(git log -1 --format=%h "$sha"); fix it first" ;;
    *) die "CI still running on $(git log -1 --format=%h "$sha") ($ci); try again when it's done" ;;
  esac
fi

# Latest release tag by semver (prerelease < stable).
latest=$(git tag -l 'v*' | while read -r t; do k=$(parse "$t") && echo "$k $t"; done |
  sort -k1,1n -k2,2n -k3,3n -k4,4n -k5,5n | tail -1)

if [[ -z $latest ]]; then
  base=0.1.0
  [[ $bump =~ ^[0-9] ]] && base=$bump
  n=1
else
  read -r M m p lrank ln latest_tag <<<"$latest"
  case $bump in
    "") ((lrank < 2)) && base="$M.$m.$p" || base="$M.$m.$((p + 1))" ;;
    patch) base="$M.$m.$((p + 1))" ;;
    minor) base="$M.$((m + 1)).0" ;;
    major) base="$((M + 1)).0.0" ;;
    *) base=$bump ;;
  esac
  if [[ $base == "$M.$m.$p" ]]; then
    ((lrank < 2)) || die "$latest_tag is already released; pass a bump (patch/minor/major)"
    ((rank >= lrank)) || die "$latest_tag is already $(channel_of "$lrank"); can't go back to $channel without a bump"
    ((rank == lrank)) && n=$((ln + 1)) || n=1
  else
    ver_gt "$base" "$M.$m.$p" || die "$base is not newer than $latest_tag"
    n=1
  fi
fi

tag=v$base
[[ $channel == stable ]] || tag+="-$channel.$n"
git rev-parse -q --verify "refs/tags/$tag" >/dev/null && die "tag $tag already exists"

# A commit already carrying a tag of the same or a higher channel would be
# released twice under different names.
for t in $(git tag --points-at "$sha" -l 'v*'); do
  k=$(parse "$t") || continue
  read -r _ _ _ trank _ <<<"$k"
  ((trank < rank)) || die "$t already points at this commit; commit something new (or promote to a higher channel)"
done

echo
echo "  release:  $tag ($channel)"
echo "  commit:   $(git log -1 --format='%h %s' "$sha")"
echo "  previous: ${latest_tag:-none}"
((promote)) || echo "  main:     fast-forward to devel"
echo
if ((!yes)); then
  read -rp "push it? [y/N] " ok
  [[ $ok == [yY]* ]] || die "aborted"
fi

((promote)) || git push origin "$sha:refs/heads/main"
git tag -a "$tag" -m "Tramite $tag ($channel)" "$sha"
git push origin "refs/tags/$tag"
git fetch -q origin
if git rev-parse -q --verify main >/dev/null && [[ $(git branch --show-current) != main ]]; then
  git branch -f main origin/main
fi
echo "pushed $tag; the release workflow builds, pushes and deploys it."
