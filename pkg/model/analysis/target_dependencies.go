package analysis

import (
	"context"
	"fmt"
	"sort"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/storage/object"
)

// ComputeTargetDependenciesValue reports the targets that a target
// refers to through its label-typed attributes.
//
// This is the loading-phase dependency edge that "bazel query" walks,
// and it is deliberately not the one analysis walks. It reads the
// attribute values a target was declared with, so a configurable
// attribute contributes every branch of its select() rather than the one
// a configuration would pick: at this point there is no configuration to
// ask. A caller that wants the edges of a single configuration wants
// cquery.
func (c *baseComputer[TReference, TMetadata]) ComputeTargetDependenciesValue(ctx context.Context, key *model_analysis_pb.TargetDependencies_Key, e TargetDependenciesEnvironment[TReference, TMetadata]) (PatchedTargetDependenciesValue[TMetadata], error) {
	targetLabel, err := label.NewCanonicalLabel(key.Label)
	if err != nil {
		return PatchedTargetDependenciesValue[TMetadata]{}, fmt.Errorf("invalid target label %#v: %w", key.Label, err)
	}
	targetValue := e.GetTargetValue(&model_analysis_pb.Target_Key{Label: key.Label})
	if !targetValue.IsSet() {
		return PatchedTargetDependenciesValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	labels := map[string]struct{}{}
	switch definition := targetValue.Message.Definition.GetKind().(type) {
	case *model_starlark_pb.Target_Definition_RuleTarget:
		fromPackage := targetLabel.GetCanonicalPackage()
		for i, attrValue := range definition.RuleTarget.PublicAttrValues {
			for j, group := range attrValue.ValueParts {
				if err := c.collectLabelsFromSelectGroup(
					ctx,
					model_core.Nested(targetValue, group),
					fromPackage,
					labels,
				); err != nil {
					return PatchedTargetDependenciesValue[TMetadata]{}, fmt.Errorf("attribute at index %d, value part %d: %w", i, j, err)
				}
			}
		}
	case *model_starlark_pb.Target_Definition_Alias:
		// An alias contributes the target it points at, which is how
		// query reports it. Its "actual" is itself a select group, so
		// an alias that is configurable contributes every branch, on
		// the same terms as any other configurable attribute. Source
		// files, package groups and predeclared outputs contribute
		// nothing.
		if err := c.collectLabelsFromSelectGroup(
			ctx,
			model_core.Nested(targetValue, definition.Alias.GetActual()),
			targetLabel.GetCanonicalPackage(),
			labels,
		); err != nil {
			return PatchedTargetDependenciesValue[TMetadata]{}, err
		}
	}

	sorted := make([]string, 0, len(labels))
	for l := range labels {
		sorted = append(sorted, l)
	}
	sort.Strings(sorted)
	return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.TargetDependencies_Value{
		Labels: sorted,
	}), nil
}

func (c *baseComputer[TReference, TMetadata]) collectLabelsFromSelectGroup(ctx context.Context, group model_core.Message[*model_starlark_pb.Select_Group, TReference], fromPackage label.CanonicalPackage, labels map[string]struct{}) error {
	if group.Message == nil {
		return nil
	}
	for _, condition := range group.Message.Conditions {
		// The condition identifier is itself a label: a config_setting
		// that the target depends on being loadable.
		if condition.ConditionIdentifier != "" {
			if err := addDependencyLabel(condition.ConditionIdentifier, fromPackage, labels); err != nil {
				return err
			}
		}
		if err := c.collectLabelsFromValue(ctx, model_core.Nested(group, condition.Value), fromPackage, labels); err != nil {
			return err
		}
	}
	if noMatch, ok := group.Message.NoMatch.(*model_starlark_pb.Select_Group_NoMatchValue); ok {
		if err := c.collectLabelsFromValue(ctx, model_core.Nested(group, noMatch.NoMatchValue), fromPackage, labels); err != nil {
			return err
		}
	}
	return nil
}

func (c *baseComputer[TReference, TMetadata]) collectLabelsFromValue(ctx context.Context, value model_core.Message[*model_starlark_pb.Value, TReference], fromPackage label.CanonicalPackage, labels map[string]struct{}) error {
	if value.Message == nil {
		return nil
	}
	switch kind := value.Message.Kind.(type) {
	case *model_starlark_pb.Value_Label:
		return addDependencyLabel(kind.Label, fromPackage, labels)
	case *model_starlark_pb.Value_List:
		var errIter error
		for element := range model_starlark.AllListLeafElementsSkippingDuplicateParents(
			ctx,
			c.valueReaders.List,
			model_core.Nested(value, kind.List.Elements),
			map[model_core.Decodable[object.LocalReference]]struct{}{},
			&errIter,
		) {
			if err := c.collectLabelsFromValue(ctx, element, fromPackage, labels); err != nil {
				return err
			}
		}
		return errIter
	case *model_starlark_pb.Value_Select:
		for _, group := range kind.Select.Groups {
			if err := c.collectLabelsFromSelectGroup(ctx, model_core.Nested(value, group), fromPackage, labels); err != nil {
				return err
			}
		}
		return nil
	}
	// Strings are NOT treated as labels. Bazel's query only reports
	// label-typed attributes, and guessing at strings that happen to
	// look like labels would invent edges that do not exist.
	return nil
}

// addDependencyLabel resolves a label found in an attribute value
// relative to the package that declared it.
//
// A reference that names a repo by its APPARENT name is recorded as
// written rather than resolved to a canonical repo, which would need a
// CanonicalRepoName lookup per distinct repo. Within one repo -- which
// is where a first-party rdeps() query operates -- AppendLabel already
// yields the canonical form, because the package it is appended to is
// canonical.
func addDependencyLabel(value string, fromPackage label.CanonicalPackage, labels map[string]struct{}) error {
	resolved, err := fromPackage.AppendLabel(value)
	if err != nil {
		// An attribute may legitimately hold a string that is not a
		// label, so this contributes no edge rather than failing the
		// whole query.
		return nil
	}
	labels[resolved.String()] = struct{}{}
	return nil
}
