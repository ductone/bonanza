"""Test rules used to exercise "bonanza_bazel test"."""

def _shell_test_impl(ctx):
    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(
        output = script,
        content = """#!/bin/sh
echo "{name} ran"
echo "target=$TEST_TARGET"
echo "filter=$TESTBRIDGE_TEST_ONLY"
if [ "{check_runfile}" = "True" ]; then
    cat "$TEST_SRCDIR/testproject+/action_edges_template.txt" || exit 1
fi
if [ -n "$TEST_TOTAL_SHARDS" ]; then
    echo "shard=$TEST_SHARD_INDEX/$TEST_TOTAL_SHARDS"
    if [ "{write_shard_status}" = "True" ]; then
        : > "$TEST_SHARD_STATUS_FILE"
    fi
fi
failures=0
if [ "{exit_code}" -ne 0 ] ||
   ( [ "{failure_shard}" -ge 0 ] && [ "$TEST_SHARD_INDEX" = "{failure_shard}" ] ); then
    failures=1
fi
if [ -n "$XML_OUTPUT_FILE" ]; then
    printf '<testsuite name="{name}" tests="1" failures="%s"/>\\n' "$failures" > "$XML_OUTPUT_FILE"
    echo "xml=$XML_OUTPUT_FILE"
fi
if [ "{failure_shard}" -ge 0 ] && [ "$TEST_SHARD_INDEX" = "{failure_shard}" ]; then
    echo "shard {failure_shard} failed"
    exit 1
fi
ls "$TEST_SRCDIR" || true
if [ -n "{expected_filter}" ] && [ "$TESTBRIDGE_TEST_ONLY" != "{expected_filter}" ]; then
    echo "expected filter {expected_filter}, got $TESTBRIDGE_TEST_ONLY"
    exit 1
fi
exit {exit_code}
""".format(
            exit_code = ctx.attr.exit_code,
            expected_filter = ctx.attr.expected_filter,
            check_runfile = ctx.attr.check_runfile,
            failure_shard = ctx.attr.failure_shard,
            write_shard_status = ctx.attr.write_shard_status,
            name = ctx.label.name,
        ),
        is_executable = True,
    )
    return [DefaultInfo(
        executable = script,
        files = depset([script]),
        runfiles = ctx.runfiles(files = ctx.files.data),
    )]

shell_test = rule(
    _shell_test_impl,
    attrs = {
        "data": attr.label_list(allow_files = True),
        "exit_code": attr.int(default = 0),
        "check_runfile": attr.bool(default = False),
        "failure_shard": attr.int(default = -1),
        "write_shard_status": attr.bool(default = True),
        "expected_filter": attr.string(),
    },
    test = True,
)
