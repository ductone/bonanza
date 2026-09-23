#!/bin/bash
# End-to-end smoke test for Bonanza.
#
# Builds the demo cluster and bonanza_bazel from the current checkout,
# launches the cluster with isolated state in a temporary directory,
# and uses bonanza_bazel to build a small self-verifying Bazel project
# against it (see testproject/). The project's :verify target greps the
# artifacts produced by the other targets on the worker, so a
# successful build proves analysis, remote action execution, and
# artifact contents all at once. The build is run twice to demonstrate
# that the second invocation is served from the evaluation cache.
# Afterwards the artifacts that the client wrote to the local system are
# inspected, covering the output materialization path as well.
#
# Usage:
#   tools/e2e/run.sh
#
# Environment variables:
#   E2E_RUN_DIR       State directory (default: mktemp under /tmp).
#   E2E_KEEP_CLUSTER  If set to 1, leave the cluster running on exit.
#
# The demo deployment binds fixed TCP diagnostics ports (9980-9984), so
# only one cluster can run on a host at a time.
#
# Module dependencies of the test project (bazel_skylib, platforms,
# rules_rust) are provided as local overrides cloned from git rather
# than downloaded as release archives, so this also works in sandboxes
# whose egress policy blocks GitHub release downloads. rules_rust is
# pinned to 0.62.0 with the same extension-name patch that bb-storage's
# bonanza branch applies, as newer versions declare module extensions
# that Bonanza cannot distinguish yet.
set -euo pipefail

log() { echo "e2e: $*" >&2; }
die() {
  log "FAILED: $*"
  exit 1
}

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_DIR="${E2E_RUN_DIR:-$(mktemp -d /tmp/bonanza-e2e.XXXXXX)}"
BCR=https://raw.githubusercontent.com/bazelbuild/bazel-central-registry/main
log "state directory: $RUN_DIR"

# --- preflight ---------------------------------------------------------
if [ "$(uname -s)" = "Linux" ] && ! command -v fusermount >/dev/null 2>&1; then
  log "installing fuse (bonanza_worker needs fusermount)"
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends fuse >&2 ||
    die "fusermount is unavailable and could not be installed"
fi
if [ -S "$RUN_DIR/bonanza_demo/bonanza_scheduler_clients.sock" ]; then
  die "a cluster already appears to be running in $RUN_DIR"
fi

# --- build the cluster and client from this checkout -------------------
log "building //deployments/demo and //cmd/bonanza_bazel"
(cd "$REPO" && bazel build //deployments/demo //cmd/bonanza_bazel) ||
  die "bazel build of the demo deployment failed"
CLIENT="$REPO/bazel-bin/cmd/bonanza_bazel/bonanza_bazel_/bonanza_bazel"
DEMO="$REPO/bazel-bin/deployments/demo/demo"
[ -x "$CLIENT" ] || die "client not found at $CLIENT"
[ -x "$DEMO" ] || die "demo launcher not found at $DEMO"

# --- client-side module overrides ---------------------------------------
# Cloned from git at the exact versions instead of downloading release
# archives. The registry's MODULE.bazel is overlaid, as Bazel would do
# for registry-provided archives.
DEPS="$RUN_DIR/deps"
provide_module() { # name, github repo, git ref, registry version
  local dest="$DEPS/$1"
  [ -d "$dest" ] && return 0
  log "providing module $1@$4 from github.com/$2@$3"
  git clone --quiet --depth 1 --branch "$3" "https://github.com/$2.git" "$dest" ||
    die "clone of $2 failed"
  curl -sf "$BCR/modules/$1/$4/MODULE.bazel" -o "$dest/MODULE.bazel" ||
    die "fetching registry MODULE.bazel for $1@$4 failed"
}
provide_module bazel_skylib bazelbuild/bazel-skylib 1.9.0 1.9.0
provide_module platforms bazelbuild/platforms 1.0.0 1.0.0
provide_module rules_python bazel-contrib/rules_python 2.0.3 2.0.3
provide_module rules_rust bazelbuild/rules_rust 0.62.0 0.62.0
if ! grep -q '"j"' "$DEPS/rules_rust/MODULE.bazel"; then
  patch -p0 -s --no-backup-if-mismatch -d "$DEPS/rules_rust" \
    < "$REPO/tools/e2e/patches/rules_rust-extension-name.diff" ||
    die "patching rules_rust failed"
fi

# --- test project --------------------------------------------------------
PROJECT="$RUN_DIR/testproject"
rm -rf "$PROJECT"
cp -r "$REPO/tools/e2e/testproject" "$PROJECT"
cat > "$PROJECT/.bazelrc" <<EOF
common:bonanza --remote_cache=unix://$RUN_DIR/bonanza_demo/bonanza_storage_frontend.sock
common:bonanza --remote_executor=unix://$RUN_DIR/bonanza_demo/bonanza_scheduler_clients.sock
common:bonanza --remote_encryption_key=U3YDUwfejfiRDeD4aqoR7A==
common:bonanza --remote_executor_builder_pkix_public_key=MCowBQYDK2VuAyEAE+onXE9lGj+1ykKMdYJ7ORbbGvDg6mXwX9H90afmdDI=
common:bonanza --remote_executor_fetcher_pkix_public_key=MCowBQYDK2VuAyEA4TFZl07r2DStbhdLuI3C6zU36syOXo0K9WXFOthelW4=
common:bonanza --remote_executor_client_private_key=$REPO/deployments/demo/bonanza_bazel.key.pem
common:bonanza --remote_executor_client_certificate_chain=$REPO/deployments/demo/bonanza_bazel.cert.pem
common:bonanza --override_module=bazel_tools=$REPO/starlark/bazel_tools
common:bonanza --override_module=builtins_bzl=$REPO/starlark/builtins_bzl
common:bonanza --override_module=builtins_core=$REPO/starlark/builtins_core
common:bonanza --override_module=bazel_skylib=$DEPS/bazel_skylib
common:bonanza --override_module=platforms=$DEPS/platforms
common:bonanza --override_module=rules_python=$DEPS/rules_python
common:bonanza --override_module=rules_rust=$DEPS/rules_rust
common:bonanza --builtins_module=builtins_core
common:bonanza --builtins_module=builtins_bzl
common:bonanza --rule_implementation_wrapper_identifier=@@builtins_core+//:wrappers.bzl%invoke_rule
common:bonanza --subrule_implementation_wrapper_identifier=@@builtins_core+//:wrappers.bzl%invoke_subrule
common:bonanza --repo_platform=//platforms:repo
common:bonanza --registry=$BCR
EOF

# --- launch the cluster --------------------------------------------------
log "launching demo cluster"
setsid env HOME="$RUN_DIR" "$DEMO" > "$RUN_DIR/cluster.log" 2>&1 &
CLUSTER_PID=$!

teardown() {
  trap - EXIT INT TERM
  if [ "${E2E_KEEP_CLUSTER:-}" = "1" ]; then
    log "leaving cluster running (E2E_KEEP_CLUSTER=1); state in $RUN_DIR"
    return
  fi
  log "tearing down cluster"
  kill -TERM -- "-$CLUSTER_PID" 2>/dev/null || true
  for _ in $(seq 20); do
    kill -0 "$CLUSTER_PID" 2>/dev/null || break
    sleep 1
  done
  kill -KILL -- "-$CLUSTER_PID" 2>/dev/null || true
  umount "$RUN_DIR/bonanza_demo/bonanza_worker_mount" 2>/dev/null || true
}
trap teardown EXIT INT TERM

for _ in $(seq 60); do
  [ -S "$RUN_DIR/bonanza_demo/bonanza_scheduler_clients.sock" ] &&
    [ -S "$RUN_DIR/bonanza_demo/bonanza_scheduler_workers.sock" ] &&
    [ -S "$RUN_DIR/bonanza_demo/bonanza_storage_frontend.sock" ] && break
  grep -q "Fatal error" "$RUN_DIR/cluster.log" && {
    tail -5 "$RUN_DIR/cluster.log" >&2
    die "cluster failed to start"
  }
  sleep 1
done
[ -S "$RUN_DIR/bonanza_demo/bonanza_scheduler_clients.sock" ] ||
  die "cluster sockets did not appear; see $RUN_DIR/cluster.log"
for _ in $(seq 60); do
  if mount 2>/dev/null | grep -q "$RUN_DIR/bonanza_demo/bonanza_worker_mount"; then
    break
  fi
  grep -q "Fatal error" "$RUN_DIR/cluster.log" && {
    tail -5 "$RUN_DIR/cluster.log" >&2
    die "worker failed to start"
  }
  sleep 1
done
log "cluster is up"

# --- build the test project ----------------------------------------------
# HOME is pointed at the state directory so that any ~/.bazelrc of the
# invoking user (which may contain flags bonanza_bazel does not accept)
# is not picked up.
build() {
  (cd "$PROJECT" && HOME="$RUN_DIR" "$CLIENT" build --config=bonanza //:all)
}
log "building //:all with bonanza_bazel (cold)"
t0=$(date +%s)
# The worker synchronizes with the scheduler on its own cadence, so on
# slow machines the first build request may arrive before any worker
# has registered. Retry while that is the only failure observed.
for attempt in $(seq 6); do
  if build 2> "$RUN_DIR/build_stderr.log"; then
    cat "$RUN_DIR/build_stderr.log" >&2
    break
  fi
  cat "$RUN_DIR/build_stderr.log" >&2
  grep -q "No workers exist" "$RUN_DIR/build_stderr.log" ||
    die "bonanza_bazel build failed"
  [ "$attempt" -lt 6 ] || die "bonanza_bazel build failed: no workers registered"
  log "no workers registered yet; retrying in 5s (attempt $attempt)"
  sleep 5
done
t1=$(date +%s)
log "cold build succeeded in $((t1 - t0))s"

# --- check that the artifacts were written to the local system ---------
# The client materializes the output files of every target that was
# built into an output directory, using the same naming scheme that is
# used for action input roots.
OUT="$PROJECT/bonanza-out"
[ -d "$OUT" ] || die "no output directory was created at $OUT"
verify_output() { # relative path, expected substring
  local f="$OUT/$1"
  [ -f "$f" ] || die "expected output file $f to exist"
  grep -q "$2" "$f" || die "output file $f does not contain $2"
}
BIN_DIRS=("$OUT"/bazel-out/*/bin/external/testproject+)
[ -d "${BIN_DIRS[0]}" ] ||
  die "no output files were written below $OUT/bazel-out"
BIN="${BIN_DIRS[0]#"$OUT"/}"
verify_output "$BIN/hello.txt" "Hello from patched bonanza"
verify_output "$BIN/verify.txt" "Hello from patched bonanza"
verify_output "external/testproject+/action_edges_template.txt" "name={NAME}"
[ -L "$PROJECT/bonanza-bin" ] ||
  die "no bonanza-bin convenience symlink was created"
log "artifacts were materialized below $OUT"

log "building //:all with bonanza_bazel (warm; should be served from the evaluation cache)"
t0=$(date +%s)
build || die "warm bonanza_bazel build failed"
t1=$(date +%s)
log "warm build succeeded in $((t1 - t0))s"

# --- run the tests -------------------------------------------------------
# Both directions are checked. A "test" that builds its targets and skips
# execution would pass the first assertion and fail the second, which is
# the failure mode worth catching.
test_target() {
  (cd "$PROJECT" && HOME="$RUN_DIR" "$CLIENT" test --config=bonanza "$1")
}
log "running //:passing_test with bonanza_bazel test"
test_target //:passing_test || die "bonanza_bazel test of a passing test failed"
log "passing test passed"

log "running //:failing_test with bonanza_bazel test (expected to fail)"
if test_target //:failing_test 2> "$RUN_DIR/failing_test_stderr.log"; then
  die "bonanza_bazel test of a failing test unexpectedly succeeded"
fi
log "failing test failed, as expected"

log "PASSED"
