"""Standalone regression fixture for rules_go and rules_python legacy runfiles collection."""

def _leaf_impl(ctx):
    data = ctx.actions.declare_file(ctx.label.name + "_data.txt")
    default = ctx.actions.declare_file(ctx.label.name + "_default.txt")
    ctx.actions.write(data, "data\n")
    ctx.actions.write(default, "default\n")
    return [DefaultInfo(
        files = depset([default]),
        data_runfiles = ctx.runfiles(files = [data]),
        default_runfiles = None if ctx.attr.data_only else ctx.runfiles(files = [default]),
    )]

leaf = rule(
    implementation = _leaf_impl,
    attrs = {"data_only": attr.bool(default = False)},
)

def _probe_impl(ctx):
    default = ctx.runfiles(collect_default = True)
    data = ctx.runfiles(collect_data = True)
    leaf_info = ctx.attr.data[0][DefaultInfo]
    output = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(output, "default=" + ",".join(sorted([f.short_path for f in default.files.to_list()])) + "\n" +
                           "data=" + ",".join(sorted([f.short_path for f in data.files.to_list()])) + "\n" +
                           "leaf_data=" + ",".join(sorted([f.short_path for f in leaf_info.data_runfiles.files.to_list()])) + "\n" +
                           "leaf_default=" + ",".join(sorted([f.short_path for f in leaf_info.default_runfiles.files.to_list()])) + "\n")
    return [DefaultInfo(files = depset([output]))]

probe = rule(
    implementation = _probe_impl,
    attrs = {
        "data": attr.label_list(allow_files = True),
        "deps": attr.label_list(allow_files = True),
        "srcs": attr.label_list(allow_files = True),
    },
)

def _map_import(value):
    return value

def _template_impl(ctx):
    output = ctx.actions.declare_file(ctx.label.name + ".txt")
    computed = ctx.actions.template_dict()
    computed.add_joined(
        "%imports%",
        depset(["pkg/a", "pkg/b"]),
        join_with = ":",
        map_each = _map_import,
    )
    ctx.actions.expand_template(
        template = ctx.file.template,
        output = output,
        substitutions = {"%workspace_name%": ctx.workspace_name},
        computed_substitutions = computed,
    )
    return [DefaultInfo(files = depset([output]))]

template_probe = rule(
    implementation = _template_impl,
    attrs = {"template": attr.label(allow_single_file = True, mandatory = True)},
)
