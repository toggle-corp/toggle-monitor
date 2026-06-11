#!/usr/bin/env bash
# Tests for release_custom_hook (defined in ../release.sh).
#
# release.sh is structured so its bottom "exec fugit" is guarded — sourcing
# this file just defines the hook function without launching the release
# flow, which is what we need for isolated testing.
#
# Each test sets up a fixture Chart.yaml in a tempdir, exports `version_tag`
# (the way fugit's scripts/release.sh would), and calls release_custom_hook
# with the chart path pointed at the fixture. Pass/fail is asserted by exit
# status of the function and grep-based content checks.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=/dev/null
source "$REPO_ROOT/release.sh"

PASS=0
FAIL=0

fail() {
    printf '  \033[31mFAIL\033[0m: %s\n' "$1"
    FAIL=$((FAIL + 1))
}

pass() {
    printf '  \033[32mPASS\033[0m: %s\n' "$1"
    PASS=$((PASS + 1))
}

make_chart() {
    local dir="$1"
    mkdir -p "$dir"
    cat >"$dir/Chart.yaml" <<'EOF'
apiVersion: v2
name: toggle-monitor-helm
description: Test fixture.
type: application
version: 0.1.1  # managed by release.sh
appVersion: "0.1.0"  # managed by release.sh
EOF
}

TMPROOT=$(mktemp -d)
trap 'rm -rf "$TMPROOT"' EXIT

run_test() {
    local name="$1"
    shift
    printf '\n%s\n' "$name"
    if "$@"; then
        pass "$name"
    else
        fail "$name"
    fi
}

# --- Test 1: hook bumps `version:` line --------------------------------------
test_bumps_version() {
    local tmp="$TMPROOT/t1"
    make_chart "$tmp"
    CHART_YAML_PATH="$tmp/Chart.yaml" \
    version_tag="9.9.9" \
        release_custom_hook

    grep -q '^version: 9.9.9  # managed by release.sh$' "$tmp/Chart.yaml"
}

run_test "bumps version: line" test_bumps_version

# --- Test 2: hook bumps `appVersion:` line -----------------------------------
test_bumps_appVersion() {
    local tmp="$TMPROOT/t2"
    make_chart "$tmp"
    CHART_YAML_PATH="$tmp/Chart.yaml" \
    version_tag="9.9.9" \
        release_custom_hook

    grep -q '^appVersion: "9.9.9"  # managed by release.sh$' "$tmp/Chart.yaml"
}

run_test "bumps appVersion: line" test_bumps_appVersion

# --- Test 3: hook fails loudly when marker is missing ------------------------
test_fails_when_marker_missing() {
    local tmp="$TMPROOT/t3"
    make_chart "$tmp"
    # Strip the marker from the version line — simulates a hand-edit that
    # removed the comment. The hook must refuse to proceed silently.
    sed -i.bak 's|^version: \(.*\)  # managed by release.sh$|version: \1|' "$tmp/Chart.yaml"
    rm -f "$tmp/Chart.yaml.bak"

    # Expect non-zero exit.
    if CHART_YAML_PATH="$tmp/Chart.yaml" \
       version_tag="9.9.9" \
       release_custom_hook 2>/dev/null; then
        return 1   # FAIL — hook should have errored
    fi
}

run_test "fails when marker missing" test_fails_when_marker_missing

# --- Test 4: hook fails when chart file is absent ----------------------------
test_fails_when_chart_missing() {
    if CHART_YAML_PATH="$TMPROOT/does-not-exist/Chart.yaml" \
       version_tag="9.9.9" \
       release_custom_hook 2>/dev/null; then
        return 1   # FAIL — hook should have errored
    fi
}

run_test "fails when chart file absent" test_fails_when_chart_missing

# --- Test 5: hook works on a copy of the production Chart.yaml ---------------
# Guards against fixture drift: if the real chart loses its marker, this
# test fails even though tests 1-4 (which use an inline fixture) still pass.
test_works_on_real_chart() {
    local tmp="$TMPROOT/t5"
    mkdir -p "$tmp"
    cp "$REPO_ROOT/deploy/helm/toggle-monitor/Chart.yaml" "$tmp/Chart.yaml"

    CHART_YAML_PATH="$tmp/Chart.yaml" \
    version_tag="9.9.9" \
        release_custom_hook

    grep -q '^version: 9.9.9  # managed by release.sh$' "$tmp/Chart.yaml" &&
        grep -q '^appVersion: "9.9.9"  # managed by release.sh$' "$tmp/Chart.yaml"
}

run_test "works on real Chart.yaml copy" test_works_on_real_chart

# --- Summary ----------------------------------------------------------------
printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
