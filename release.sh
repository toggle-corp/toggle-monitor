#!/usr/bin/env bash
# Parent release wrapper. Drives fugit (vendored at ./fugit) to cut a
# tagged release for this repo. Run from a clean main branch:
#
#   ./release.sh           # prompts for the version
#   ./release.sh 0.2.0     # pre-fills the prompt
#
# Configuration is via env vars (see fugit/CLAUDE.md for the full list);
# we keep this wrapper thin and lean on fugit for the heavy lifting.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export SCRIPT_DIR

# Chart manifest whose version: / appVersion: lines we keep in lockstep
# with the git tag (see release_custom_hook below). Override-able for
# unit tests; defaults to the production chart in this repo.
: "${CHART_YAML_PATH:=$SCRIPT_DIR/deploy/helm/toggle-monitor/Chart.yaml}"
export CHART_YAML_PATH

# fugit invokes "$RELEASE_CUSTOM_HOOK" (a function name, no args) AFTER
# it has read the new version into the local `version_tag` variable but
# BEFORE it writes CHANGELOG.md and creates the tag. We use it to bump
# Chart.yaml so the chart version baked into the tag matches the tag
# itself. The function reads `$version_tag` via bash dynamic scoping
# from fugit's calling shell.
release_custom_hook() {
    local new_version="$version_tag"
    local chart="$CHART_YAML_PATH"

    sed -i.bak -E \
        -e "s|^(version: )[^ ]+( +# managed by release\.sh)$|\1${new_version}\2|" \
        -e "s|^(appVersion: )\"[^\"]+\"( +# managed by release\.sh)$|\1\"${new_version}\"\2|" \
        "$chart"
    rm -f "${chart}.bak"

    # Defence in depth: if the marker was removed by hand, the sed above
    # is a silent no-op. Refuse to proceed without proof both lines landed.
    if ! grep -q "^version: ${new_version}  # managed by release\.sh$" "$chart"; then
        echo "release_custom_hook: 'version: ${new_version}  # managed by release.sh' not found in $chart" >&2
        echo "  The '# managed by release.sh' marker was probably removed by hand." >&2
        return 1
    fi
    if ! grep -q "^appVersion: \"${new_version}\"  # managed by release\.sh$" "$chart"; then
        echo "release_custom_hook: 'appVersion: \"${new_version}\"  # managed by release.sh' not found in $chart" >&2
        echo "  The '# managed by release.sh' marker was probably removed by hand." >&2
        return 1
    fi

    helm lint "$(dirname "$chart")"
}
export -f release_custom_hook
export RELEASE_CUSTOM_HOOK=release_custom_hook

# Fugit needs a handful of repo-shape env vars and one git-cliff template
# config. START_COMMIT is the oldest commit fugit will include in change
# generation — we anchor it on the repo's first commit so every release's
# changelog reflects the full history slice since the previous tag.
: "${START_COMMIT:=$(git -C "$SCRIPT_DIR" rev-list --max-parents=0 HEAD 2>/dev/null || echo HEAD)}"
export START_COMMIT
export REPO_NAME=toggle-corp/toggle-monitor
export DEFAULT_BRANCH=main
export FUGIT_HOST_TYPE=github
export VERSION_TAG_PREFIX_MODE=forbid
export GIT_CLIFF__REMOTE__GITHUB__OWNER=toggle-corp
export GIT_CLIFF__REMOTE__GITHUB__REPO=toggle-monitor

run_preflight() {
    echo "==> pre-flight: go build"
    (cd "$SCRIPT_DIR" && go build ./...)
    echo "==> pre-flight: go test"
    (cd "$SCRIPT_DIR" && go test ./...)
    echo "==> pre-flight: helm lint"
    helm lint "$SCRIPT_DIR/deploy/helm/toggle-monitor"
}

# Only fire the release flow when invoked directly. Sourcing this file
# (e.g. from scripts/test-release-hook.sh) leaves the function defined
# and env exported without launching fugit.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    if [[ ! -x "$SCRIPT_DIR/fugit/scripts/release.sh" ]]; then
        echo "release.sh: fugit submodule missing at $SCRIPT_DIR/fugit/" >&2
        echo "  Run: git submodule update --init --recursive" >&2
        exit 1
    fi
    run_preflight
    exec "$SCRIPT_DIR/fugit/scripts/release.sh" "$@"
fi
