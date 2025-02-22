#!/usr/bin/env bash
#
# This script is run by the Pebble Nightly Metamorphic - TeamCity build
# configuration.

set -euo pipefail

dir="$(dirname $(dirname $(dirname $(dirname "${0}"))))"

source "$dir/teamcity-support.sh"  # For $root
source "$dir/teamcity-bazel-support.sh"  # For run_bazel

mkdir -p artifacts

# Pull in the latest version of Pebble from upstream. The benchmarks run
# against the tip of the 'master' branch. We do this by `go get`ting the
# latest version of the module, and then running `mirror` to update `DEPS.bzl`
# accordingly.
bazel run @go_sdk//:bin/go get github.com/cockroachdb/pebble@latest
NEW_DEPS_BZL_CONTENT=$(bazel run //pkg/cmd/mirror)
echo "$NEW_DEPS_BZL_CONTENT" > DEPS.bzl

PEBBLE_SUM=$(grep 'version =' DEPS.bzl | cut -d'"' -f2)
echo "Pebble module sum: $PEBBLE_SUM"

env=(
  "GITHUB_REPO=$GITHUB_REPO"
  "GITHUB_API_TOKEN=$GITHUB_API_TOKEN"
  "BUILD_VCS_NUMBER=$PEBBLE_SUM"
  "TC_BUILD_ID=$TC_BUILD_ID"
  "TC_SERVER_URL=$TC_SERVER_URL"
  "TC_BUILD_BRANCH=$TC_BUILD_BRANCH"
  "PKG=internal/metamorphic"
  "STRESSFLAGS=-maxtime 3h -maxfails 1 -stderr -p 1"
  "TZ=America/New_York"
)

BAZEL_SUPPORT_EXTRA_DOCKER_ARGS="-e BUILD_VCS_NUMBER=$PEBBLE_SUM -e GITHUB_API_TOKEN -e GITHUB_REPO -e TC_BUILD_BRANCH -e TC_BUILD_ID -e TC_SERVER_URL" \
                               run_bazel build/teamcity/cockroach/nightlies/pebble_nightly_metamorphic_impl.sh

