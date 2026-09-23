"""Test rules used to exercise "bonanza_bazel test"."""

def _shell_test_impl(ctx):
    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(
        output = script,
        content = """#!/bin/sh
echo "{name} ran"
echo "target=$TEST_TARGET"
echo "filter=$TESTBRIDGE_TEST_ONLY"
ls "$TEST_SRCDIR" || true
if [ -n "{expected_filter}" ] && [ "$TESTBRIDGE_TEST_ONLY" != "{expected_filter}" ]; then
    echo "expected filter {expected_filter}, got $TESTBRIDGE_TEST_ONLY"
    exit 1
fi
exit {exit_code}
""".format(
            exit_code = ctx.attr.exit_code,
            expected_filter = ctx.attr.expected_filter,
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
        "expected_filter": attr.string(),
    },
    test = True,
)
