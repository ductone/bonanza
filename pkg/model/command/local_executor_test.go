package command

import (
	"testing"

	model_command_pb "bonanza.build/pkg/proto/model/command"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNativeRunnerRejectsStatefulRepositoryActions(t *testing.T) {
	for name, command := range map[string]*model_command_pb.Command{
		"writable input tree": {NeedsWritableInputFiles: true},
		"stable input root":   {StableInputRootPathUuid: "123e4567-e89b-12d3-a456-426614174000"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateNativeCommand(command); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("native command unexpectedly accepted: %v", err)
			}
		})
	}
	if err := validateNativeCommand(&model_command_pb.Command{}); err != nil {
		t.Fatalf("stateless build action rejected: %v", err)
	}
}
