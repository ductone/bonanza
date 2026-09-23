package analysis

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
)

// ComputeConfiguredTargetDependenciesValue reports the targets a target
// refers to in a given configuration.
//
// This is cquery's edge, and the difference from TargetDependencies is
// the whole reason cquery exists: a select() contributes only the branch
// the configuration selects, rather than every branch. Each resulting
// label is then resolved through VisibleTarget, so an alias reports the
// target it expands to in this configuration rather than the alias
// itself -- again matching cquery, which reports what a build would
// actually depend on.
func (baseComputer[TReference, TMetadata]) ComputeConfiguredTargetDependenciesValue(ctx context.Context, key model_core.Message[*model_analysis_pb.ConfiguredTargetDependencies_Key, TReference], e ConfiguredTargetDependenciesEnvironment[TReference, TMetadata]) (PatchedConfiguredTargetDependenciesValue[TMetadata], error) {
	targetLabel, err := label.NewCanonicalLabel(key.Message.Label)
	if err != nil {
		return PatchedConfiguredTargetDependenciesValue[TMetadata]{}, fmt.Errorf("invalid target label %#v: %w", key.Message.Label, err)
	}
	fromPackage := targetLabel.GetCanonicalPackage()
	configurationReference := model_core.Nested(key, key.Message.ConfigurationReference)

	targetValue := e.GetTargetValue(&model_analysis_pb.Target_Key{Label: key.Message.Label})
	if !targetValue.IsSet() {
		return PatchedConfiguredTargetDependenciesValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	// Collect the labels the attributes name in this configuration.
	unresolved := map[string]struct{}{}
	missingDependencies := false
	switch definition := targetValue.Message.Definition.GetKind().(type) {
	case *model_starlark_pb.Target_Definition_RuleTarget:
		for i, attrValue := range definition.RuleTarget.PublicAttrValues {
			for j, group := range attrValue.ValueParts {
				value, err := getValueFromSelectGroup(e, configurationReference, fromPackage, group /* permitNoMatch = */, true)
				if err != nil {
					if !errors.Is(err, evaluation.ErrMissingDependency) {
						return PatchedConfiguredTargetDependenciesValue[TMetadata]{}, fmt.Errorf("attribute at index %d, value part %d: %w", i, j, err)
					}
					missingDependencies = true
					continue
				}
				collectLabelsFromResolvedValue(value, fromPackage, unresolved)
			}
		}
	case *model_starlark_pb.Target_Definition_Alias:
		value, err := getValueFromSelectGroup(e, configurationReference, fromPackage, definition.Alias.GetActual() /* permitNoMatch = */, true)
		if err != nil {
			if !errors.Is(err, evaluation.ErrMissingDependency) {
				return PatchedConfiguredTargetDependenciesValue[TMetadata]{}, err
			}
			missingDependencies = true
		} else {
			collectLabelsFromResolvedValue(value, fromPackage, unresolved)
		}
	}
	if missingDependencies {
		return PatchedConfiguredTargetDependenciesValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	// Resolve each label the way the target itself would see it, so an
	// alias reports its expansion and an invisible target reports
	// nothing.
	resolved := map[string]struct{}{}
	for l := range unresolved {
		patchedConfigurationReference := model_core.Patch(e, configurationReference)
		visibleTarget := e.GetVisibleTargetValue(
			model_core.NewPatchedMessage(
				&model_analysis_pb.VisibleTarget_Key{
					FromPackage:            fromPackage.String(),
					ToLabel:                l,
					ConfigurationReference: patchedConfigurationReference.Message,
				},
				patchedConfigurationReference.Patcher,
			),
		)
		if !visibleTarget.IsSet() {
			missingDependencies = true
			continue
		}
		if visibleTarget.Message.Label != "" {
			resolved[visibleTarget.Message.Label] = struct{}{}
		}
	}
	if missingDependencies {
		return PatchedConfiguredTargetDependenciesValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	sorted := make([]string, 0, len(resolved))
	for l := range resolved {
		sorted = append(sorted, l)
	}
	sort.Strings(sorted)
	return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.ConfiguredTargetDependencies_Value{
		Labels: sorted,
	}), nil
}

// collectLabelsFromResolvedValue gathers the labels of a value that a
// select() has already been resolved out of.
//
// Only inline list elements are followed. An attribute whose value is
// large enough to have been stored externally needs the list reader,
// which this does not take; such an attribute contributes nothing rather
// than a guess. The same limitation applies to TargetDependencies'
// treatment of externally stored lists.
func collectLabelsFromResolvedValue(value *model_starlark_pb.Value, fromPackage label.CanonicalPackage, out map[string]struct{}) {
	if value == nil {
		return
	}
	switch kind := value.Kind.(type) {
	case *model_starlark_pb.Value_Label:
		if resolved, err := fromPackage.AppendLabel(kind.Label); err == nil {
			out[resolved.String()] = struct{}{}
		}
	case *model_starlark_pb.Value_List:
		for _, element := range kind.List.Elements {
			if leaf, ok := element.Level.(*model_starlark_pb.List_Element_Leaf); ok {
				collectLabelsFromResolvedValue(leaf.Leaf, fromPackage, out)
			}
		}
	}
}
