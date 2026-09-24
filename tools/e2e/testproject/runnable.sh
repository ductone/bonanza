#!/bin/sh
# Launched by "bonanza_bazel run //:runnable" during the end-to-end test.
echo "run: args=$*"
echo "run: cwd=$(basename "$(pwd)")"
echo "run: workspace=$(basename "${BUILD_WORKSPACE_DIRECTORY:-unset}")"
echo "run: runfiles=$(basename "${RUNFILES_DIR:-unset}")"
cat "$RUNFILES_DIR/_main/runnable_data.txt"
