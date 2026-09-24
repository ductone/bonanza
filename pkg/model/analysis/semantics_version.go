package analysis

// SemanticsVersion is the version of the evaluation semantics that are
// implemented by BaseComputer and the Starlark builtins on which it
// depends. It is included in the tag keys under which workers cache
// evaluation results, so that results computed by workers implementing
// different semantics are kept separate.
//
// This constant MUST be increased whenever a change is made that causes
// evaluation of identical keys with identical dependency values to
// yield different results. Examples include altering the behavior of
// existing Starlark builtins, changing how values are encoded, or
// changing the set of dependencies that a computation requests.
//
// Changes that do not require increasing this constant include purely
// additive ones (e.g., adding new Starlark builtins, as evaluations
// that previously failed are not cached), performance improvements, and
// changes to code that does not influence the values yielded by the
// evaluation functions.
//
// Note that increasing this constant effectively discards all
// previously cached evaluation results. This is intentional, as
// dependency hashing subsequently limits recomputation to values that
// actually differ. Bumps are expected to be rare.
const SemanticsVersion = 9
