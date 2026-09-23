package query

import (
	"fmt"

	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
)

// Encode converts a parsed query expression into the form that is sent
// to the cluster for evaluation.
//
// The client parses the query language and the cluster walks the graph,
// so this is the boundary between the two. Target patterns cross it
// unresolved: canonicalizing an apparent pattern needs the repo mapping,
// which only the cluster has.
func Encode(e Expression) (*model_analysis_pb.QueryExpression, error) {
	switch x := e.(type) {
	case PatternExpression:
		return &model_analysis_pb.QueryExpression{
			Expression: &model_analysis_pb.QueryExpression_Pattern{Pattern: x.Pattern},
		}, nil
	case SetExpression:
		return &model_analysis_pb.QueryExpression{
			Expression: &model_analysis_pb.QueryExpression_Set_{
				Set: &model_analysis_pb.QueryExpression_Set{Patterns: x.Patterns},
			},
		}, nil
	case BinaryExpression:
		left, err := Encode(x.Left)
		if err != nil {
			return nil, err
		}
		right, err := Encode(x.Right)
		if err != nil {
			return nil, err
		}
		var operator model_analysis_pb.QueryExpression_Binary_Operator
		switch x.Operator {
		case SetOperatorUnion:
			operator = model_analysis_pb.QueryExpression_Binary_UNION
		case SetOperatorExcept:
			operator = model_analysis_pb.QueryExpression_Binary_EXCEPT
		case SetOperatorIntersect:
			operator = model_analysis_pb.QueryExpression_Binary_INTERSECT
		default:
			return nil, fmt.Errorf("unsupported set operator %s", x.Operator)
		}
		return &model_analysis_pb.QueryExpression{
			Expression: &model_analysis_pb.QueryExpression_Binary_{
				Binary: &model_analysis_pb.QueryExpression_Binary{
					Operator: operator,
					Left:     left,
					Right:    right,
				},
			},
		}, nil
	case DepsExpression:
		universe, err := Encode(x.Universe)
		if err != nil {
			return nil, err
		}
		return &model_analysis_pb.QueryExpression{
			Expression: &model_analysis_pb.QueryExpression_Deps_{
				Deps: &model_analysis_pb.QueryExpression_Deps{
					Universe: universe,
					Depth:    int32(x.Depth),
				},
			},
		}, nil
	case RdepsExpression:
		universe, err := Encode(x.Universe)
		if err != nil {
			return nil, err
		}
		targets, err := Encode(x.Targets)
		if err != nil {
			return nil, err
		}
		return &model_analysis_pb.QueryExpression{
			Expression: &model_analysis_pb.QueryExpression_Rdeps_{
				Rdeps: &model_analysis_pb.QueryExpression_Rdeps{
					Universe: universe,
					Targets:  targets,
					Depth:    int32(x.Depth),
				},
			},
		}, nil
	case KindExpression:
		targets, err := Encode(x.Targets)
		if err != nil {
			return nil, err
		}
		return &model_analysis_pb.QueryExpression{
			Expression: &model_analysis_pb.QueryExpression_Kind_{
				Kind: &model_analysis_pb.QueryExpression_Kind{
					Pattern: x.Pattern,
					Targets: targets,
				},
			},
		}, nil
	case FilterExpression:
		targets, err := Encode(x.Targets)
		if err != nil {
			return nil, err
		}
		return &model_analysis_pb.QueryExpression{
			Expression: &model_analysis_pb.QueryExpression_Filter_{
				Filter: &model_analysis_pb.QueryExpression_Filter{
					Pattern: x.Pattern,
					Targets: targets,
				},
			},
		}, nil
	case AttrExpression:
		targets, err := Encode(x.Targets)
		if err != nil {
			return nil, err
		}
		return &model_analysis_pb.QueryExpression{
			Expression: &model_analysis_pb.QueryExpression_Attr_{
				Attr: &model_analysis_pb.QueryExpression_Attr{
					Name:    x.Name,
					Pattern: x.Pattern,
					Targets: targets,
				},
			},
		}, nil
	}
	return nil, fmt.Errorf("unsupported query expression %T", e)
}
