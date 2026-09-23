package reapi

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	object_pb "bonanza.build/pkg/proto/storage/object"
	"bonanza.build/pkg/storage/object"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeREAPIClient struct {
	uploadedDigests []*remoteexecution.Digest
	uploadedBlobs   [][]byte
	readBlobs       map[string][]byte
	readDigests     []*remoteexecution.Digest
	executeRequest  *remoteexecution.ExecuteRequest
	response        *remoteexecution.ExecuteResponse
}

func (c *fakeREAPIClient) CheckReadiness(context.Context) error {
	return nil
}

func (c *fakeREAPIClient) UploadBlob(_ context.Context, digest *remoteexecution.Digest, contents io.Reader) error {
	data, err := io.ReadAll(contents)
	if err != nil {
		return err
	}
	c.uploadedDigests = append(c.uploadedDigests, digest)
	c.uploadedBlobs = append(c.uploadedBlobs, data)
	return nil
}

func (c *fakeREAPIClient) OpenBlob(_ context.Context, digest *remoteexecution.Digest) (io.ReadCloser, error) {
	c.readDigests = append(c.readDigests, digest)
	contents, ok := c.readBlobs[digestKey(digest)]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "missing fake REAPI blob %s", digestKey(digest))
	}
	return io.NopCloser(bytes.NewReader(contents)), nil
}

func (c *fakeREAPIClient) ReadBlob(_ context.Context, digest *remoteexecution.Digest) ([]byte, error) {
	c.readDigests = append(c.readDigests, digest)
	contents, ok := c.readBlobs[digestKey(digest)]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "missing fake REAPI blob %s", digestKey(digest))
	}
	return contents, nil
}

func (c *fakeREAPIClient) Execute(_ context.Context, request *remoteexecution.ExecuteRequest) (*remoteexecution.ExecuteResponse, error) {
	c.executeRequest = request
	return c.response, nil
}

func TestREAPIBoundaryUploadsExecutesAndImportsOutput(t *testing.T) {
	ctx := context.Background()
	referenceFormat, err := object.NewReferenceFormat(object_pb.ReferenceFormat_SHA256_V1)
	if err != nil {
		t.Fatalf("create reference format: %v", err)
	}
	fileCreationParameters, err := model_filesystem.NewFileCreationParametersFromProto(&model_filesystem_pb.FileCreationParameters{
		Access:                           &model_filesystem_pb.FileAccessParameters{},
		ChunkMinimumSizeBytes:            1024,
		FileContentsListMinimumSizeBytes: 1024,
		FileContentsListMaximumSizeBytes: 1024,
	}, referenceFormat)
	if err != nil {
		t.Fatalf("create file parameters: %v", err)
	}
	directoryCreationParameters, err := model_filesystem.NewDirectoryCreationParametersFromProto(&model_filesystem_pb.DirectoryCreationParameters{
		Access:                    &model_filesystem_pb.DirectoryAccessParameters{},
		DirectoryMaximumSizeBytes: 1 << 20,
	}, referenceFormat)
	if err != nil {
		t.Fatalf("create directory parameters: %v", err)
	}

	output := []byte("remote output")
	outputDigest := newDigest(output)
	client := &fakeREAPIClient{
		readBlobs: map[string][]byte{
			digestKey(outputDigest): output,
		},
		response: &remoteexecution.ExecuteResponse{
			Result: &remoteexecution.ActionResult{
				ExitCode: 0,
				OutputFiles: []*remoteexecution.OutputFile{{
					Path:   "out.txt",
					Digest: outputDigest,
				}},
			},
		},
	}
	executor := &executor{
		client:                        client,
		instanceName:                  "",
		objectContentsWalkerSemaphore: semaphore.NewWeighted(1),
	}

	commandDigest, err := executor.uploadProto(ctx, &remoteexecution.Command{Arguments: []string{"echo", "hello"}})
	if err != nil {
		t.Fatalf("upload REAPI command: %v", err)
	}
	if len(client.uploadedDigests) != 1 || digestKey(client.uploadedDigests[0]) != digestKey(commandDigest) {
		t.Fatalf("uploaded REAPI command digest = %#v, want %s", client.uploadedDigests, digestKey(commandDigest))
	}
	if len(client.uploadedBlobs) != 1 || len(client.uploadedBlobs[0]) == 0 {
		t.Fatalf("uploaded REAPI command bytes = %#v, want non-empty command proto", client.uploadedBlobs)
	}

	response, _, err := executor.executeREAPI(ctx, commandDigest, time.Second)
	if err != nil {
		t.Fatalf("execute fake REAPI action: %v", err)
	}
	if response != client.response {
		t.Fatalf("unexpected fake REAPI response: got %p, want %p", response, client.response)
	}
	if client.executeRequest == nil || client.executeRequest.ActionDigest == nil {
		t.Fatal("fake REAPI did not receive an Execute request with an action digest")
	}
	if client.executeRequest.DigestFunction != remoteexecution.DigestFunction_SHA256 {
		t.Fatalf("Execute digest function = %s, want SHA256", client.executeRequest.DigestFunction)
	}

	outputs, _, err := executor.importOutputs(
		ctx,
		response.Result,
		/* outputPatternSet = */ true,
		/* workingDirectoryComponents = */ nil,
		fileCreationParameters,
		directoryCreationParameters,
	)
	if err != nil {
		t.Fatalf("import fake REAPI output: %v", err)
	}
	if outputs.OutputRoot == nil {
		t.Fatal("imported REAPI output did not produce a Bonanza output root")
	}
	if len(client.readDigests) != 1 || digestKey(client.readDigests[0]) != digestKey(outputDigest) {
		t.Fatalf("REAPI output reads = %v, want %s", client.readDigests, digestKey(outputDigest))
	}
	leaves := outputs.OutputRoot.GetLeavesInline()
	if leaves == nil || len(leaves.Files) != 1 || leaves.Files[0].Name != "out.txt" {
		t.Fatalf("imported Bonanza output files = %#v, want out.txt", leaves)
	}
}

func TestNormalizeWorkingDirectoryRootAndTraversal(t *testing.T) {
	root, components, err := normalizeWorkingDirectory(".")
	if err != nil || root != "" || len(components) != 0 {
		t.Fatalf("Bonanza root working directory = %q, %v, %v; want REAPI root", root, components, err)
	}
	_, _, err = normalizeWorkingDirectory("../escape")
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("escaping working directory error = %v; want Unimplemented", err)
	}
}
