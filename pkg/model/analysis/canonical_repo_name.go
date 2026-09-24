package analysis

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
)

func (baseComputer[TReference, TMetadata]) ComputeCanonicalRepoNameValue(ctx context.Context, key *model_analysis_pb.CanonicalRepoName_Key, e CanonicalRepoNameEnvironment[TReference, TMetadata]) (PatchedCanonicalRepoNameValue[TMetadata], error) {
	fromCanonicalRepo, err := label.NewCanonicalRepo(key.FromCanonicalRepo)
	if err != nil {
		return PatchedCanonicalRepoNameValue[TMetadata]{}, fmt.Errorf("invalid canonical repo: %w", err)
	}

	toApparentRepo := key.ToApparentRepo
	if moduleExtension, _, ok := fromCanonicalRepo.GetModuleExtension(); ok {
		buildSpecification := e.GetBuildSpecificationValue(&model_analysis_pb.BuildSpecification_Key{})
		if !buildSpecification.IsSet() {
			return PatchedCanonicalRepoNameValue[TMetadata]{}, evaluation.ErrMissingDependency
		}

		// A generated repository in the validated vendor snapshot can refer
		// to its vendored siblings without evaluating the producing extension.
		// Extensions may run repository actions, even when every repository
		// needed by the target is already available locally.
		moduleInstance := fromCanonicalRepo.GetModuleInstance()
		_, hasVersion := moduleInstance.GetModuleVersion()
		locallyProvidedModule := false
		if !hasVersion {
			modules := buildSpecification.Message.Modules
			_, locallyProvidedModule = sort.Find(
				len(modules),
				func(i int) int { return strings.Compare(moduleInstance.GetModule().String(), modules[i].Name) },
			)
		}
		vendoredRepos := buildSpecification.Message.VendoredRepos
		fromIndex, fromVendored := sort.Find(
			len(vendoredRepos),
			func(i int) int { return strings.Compare(fromCanonicalRepo.String(), vendoredRepos[i].CanonicalRepo) },
		)
		fromPinned := fromVendored && vendoredRepos[fromIndex].Pinned
		if !locallyProvidedModule || fromPinned {
			canonicalSibling := moduleExtension.String() + "+" + toApparentRepo
			siblingIndex, found := sort.Find(
				len(vendoredRepos),
				func(i int) int { return strings.Compare(canonicalSibling, vendoredRepos[i].CanonicalRepo) },
			)
			if found && (!locallyProvidedModule || vendoredRepos[siblingIndex].Pinned) {
				apparentRepo, err := label.NewApparentRepo(toApparentRepo)
				if err != nil {
					return PatchedCanonicalRepoNameValue[TMetadata]{}, fmt.Errorf("invalid apparent repo: %w", err)
				}
				return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.CanonicalRepoName_Value{
					ToCanonicalRepo: moduleExtension.GetCanonicalRepoWithModuleExtension(apparentRepo).String(),
				}), nil
			}
		}

		// In strict vendor mode an absent sibling cannot be supplied by an
		// extension, even if that extension belongs to a local module. The
		// parent module's repo mapping can still resolve its dependencies.
		if !buildSpecification.Message.StrictVendorMode {
			moduleExtensionRepoNamesValue := e.GetModuleExtensionRepoNamesValue(&model_analysis_pb.ModuleExtensionRepoNames_Key{
				ModuleExtension: moduleExtension.String(),
			})
			if !moduleExtensionRepoNamesValue.IsSet() {
				return PatchedCanonicalRepoNameValue[TMetadata]{}, evaluation.ErrMissingDependency
			}
			repoNames := moduleExtensionRepoNamesValue.Message.RepoNames
			if _, found := sort.Find(
				len(repoNames),
				func(i int) int { return strings.Compare(toApparentRepo, repoNames[i]) },
			); found {
				apparentRepo, err := label.NewApparentRepo(toApparentRepo)
				if err != nil {
					return PatchedCanonicalRepoNameValue[TMetadata]{}, fmt.Errorf("invalid apparent repo: %w", err)
				}
				return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.CanonicalRepoName_Value{
					ToCanonicalRepo: moduleExtension.GetCanonicalRepoWithModuleExtension(apparentRepo).String(),
				}), nil
			}
		}
	}

	// Consider modules and repos declared in MODULE.bazel.
	moduleRepoMappingValue := e.GetModuleRepoMappingValue(&model_analysis_pb.ModuleRepoMapping_Key{
		ModuleInstance: fromCanonicalRepo.GetModuleInstance().String(),
	})
	if !moduleRepoMappingValue.IsSet() {
		return PatchedCanonicalRepoNameValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	mappings := moduleRepoMappingValue.Message.Mappings
	if mappingIndex, ok := sort.Find(
		len(mappings),
		func(i int) int { return strings.Compare(toApparentRepo, mappings[i].FromApparentRepo) },
	); ok {
		return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.CanonicalRepoName_Value{
			ToCanonicalRepo: mappings[mappingIndex].ToCanonicalRepo,
		}), nil
	}

	// Unknown repo.
	return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.CanonicalRepoName_Value{}), nil
}

type labelResolverEnvironment[TReference any] interface {
	GetCanonicalRepoNameValue(*model_analysis_pb.CanonicalRepoName_Key) model_core.Message[*model_analysis_pb.CanonicalRepoName_Value, TReference]
	GetRootModuleValue(*model_analysis_pb.RootModule_Key) model_core.Message[*model_analysis_pb.RootModule_Value, TReference]
}

// labelResolver is an implementation of label.Resolver that is built on
// top of the CanonicalRepoName and RootModule analysis functions. It is
// provided to make resolution of apparent labels and target patterns
// to their canonical counterparts work.
type labelResolver[TReference any] struct {
	environment labelResolverEnvironment[TReference]
}

func newLabelResolver[TReference any](e labelResolverEnvironment[TReference]) label.Resolver {
	return &labelResolver[TReference]{
		environment: e,
	}
}

func (r *labelResolver[TReference]) GetCanonicalRepo(fromCanonicalRepo label.CanonicalRepo, toApparentRepo label.ApparentRepo) (*label.CanonicalRepo, error) {
	v := r.environment.GetCanonicalRepoNameValue(&model_analysis_pb.CanonicalRepoName_Key{
		FromCanonicalRepo: fromCanonicalRepo.String(),
		ToApparentRepo:    toApparentRepo.String(),
	})
	if !v.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}
	if v.Message.ToCanonicalRepo == "" {
		return nil, nil
	}
	toCanonicalRepo, err := label.NewCanonicalRepo(v.Message.ToCanonicalRepo)
	if err != nil {
		return nil, fmt.Errorf("invalid canonical repo name %#v: %w", v.Message.ToCanonicalRepo, err)
	}
	return &toCanonicalRepo, nil
}

func (r *labelResolver[TReference]) GetRootModule() (label.Module, error) {
	v := r.environment.GetRootModuleValue(&model_analysis_pb.RootModule_Key{})
	var bad label.Module
	if !v.IsSet() {
		return bad, evaluation.ErrMissingDependency
	}
	rootModule, err := label.NewModule(v.Message.RootModuleName)
	if err != nil {
		return bad, fmt.Errorf("invalid root module name %#v: %w", v.Message.RootModuleName, err)
	}
	return rootModule, nil
}
