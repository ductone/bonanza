package reapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
)

type acceptingCAS struct {
	remoteexecution.ContentAddressableStorageClient
}

func (acceptingCAS) FindMissingBlobs(_ context.Context, request *remoteexecution.FindMissingBlobsRequest, _ ...grpc.CallOption) (*remoteexecution.FindMissingBlobsResponse, error) {
	return &remoteexecution.FindMissingBlobsResponse{MissingBlobDigests: request.BlobDigests}, nil
}

type writeByteStream struct {
	bytestream.ByteStreamClient
	write bytestream.ByteStream_WriteClient
}

func (s writeByteStream) Write(context.Context, ...grpc.CallOption) (bytestream.ByteStream_WriteClient, error) {
	return s.write, nil
}

type recordingWriteStream struct {
	grpc.ClientStream
	data         bytes.Buffer
	requests     []*bytestream.WriteRequest
	finished     bool
	finishOffset int64
}

func (s *recordingWriteStream) Send(request *bytestream.WriteRequest) error {
	if request == nil {
		return fmt.Errorf("ByteStream upload received no request")
	}
	if s.finished {
		return fmt.Errorf("ByteStream upload received a request after finalization")
	}
	if len(s.requests) == 0 {
		if request.ResourceName == "" {
			return fmt.Errorf("ByteStream upload's first request had no resource name")
		}
	} else if request.ResourceName != "" && request.ResourceName != s.requests[0].ResourceName {
		return fmt.Errorf("ByteStream upload's resource name changed")
	}
	if request.WriteOffset != int64(s.data.Len()) {
		return fmt.Errorf("ByteStream upload wrote at offset %d, expected %d", request.WriteOffset, s.data.Len())
	}

	s.requests = append(s.requests, request)
	if len(request.Data) > 0 {
		_, _ = s.data.Write(request.Data)
	}
	if request.FinishWrite {
		s.finished = true
		s.finishOffset = request.WriteOffset
	}
	return nil
}

func (s *recordingWriteStream) CloseAndRecv() (*bytestream.WriteResponse, error) {
	if !s.finished {
		return nil, fmt.Errorf("ByteStream upload was not finalized")
	}
	return &bytestream.WriteResponse{CommittedSize: int64(s.data.Len())}, nil
}

type earlyCompletionWriteStream struct {
	grpc.ClientStream
	committedSize int64
	request       *bytestream.WriteRequest
}

func (s *earlyCompletionWriteStream) Send(request *bytestream.WriteRequest) error {
	s.request = request
	return io.EOF
}

func (s *earlyCompletionWriteStream) CloseAndRecv() (*bytestream.WriteResponse, error) {
	return &bytestream.WriteResponse{CommittedSize: s.committedSize}, nil
}

func TestUploadBlobFinalizesAfterReaderReturnsEOFSeparately(t *testing.T) {
	payload := []byte("a Bonanza input blob")
	stream := &recordingWriteStream{}
	client := &client{
		cas:        acceptingCAS{},
		byteStream: writeByteStream{write: stream},
	}
	digest := newDigest(payload)
	require.NoError(t, client.UploadBlob(t.Context(), digest, bytes.NewReader(payload)))
	require.Equal(t, payload, stream.data.Bytes())
	require.Len(t, stream.requests, 2)
	require.NotEmpty(t, stream.requests[0].ResourceName)
	require.False(t, stream.requests[0].FinishWrite)
	require.Empty(t, stream.requests[1].ResourceName)
	require.True(t, stream.requests[1].FinishWrite)
	require.Equal(t, int64(len(payload)), stream.finishOffset)
}

func TestUploadBlobAcceptsCompletedConcurrentUpload(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), transferChunkSize+1)
	stream := &earlyCompletionWriteStream{committedSize: int64(len(payload))}
	client := &client{
		cas:        acceptingCAS{},
		byteStream: writeByteStream{write: stream},
	}

	require.NoError(t, client.UploadBlob(t.Context(), newDigest(payload), bytes.NewReader(payload)))
	require.NotNil(t, stream.request)
	require.NotEmpty(t, stream.request.ResourceName)
	require.Equal(t, payload[:transferChunkSize], stream.request.Data)
}

func TestUploadBlobRejectsIncompleteConcurrentUpload(t *testing.T) {
	payload := []byte("a Bonanza input blob")
	stream := &earlyCompletionWriteStream{committedSize: int64(len(payload) - 1)}
	client := &client{
		cas:        acceptingCAS{},
		byteStream: writeByteStream{write: stream},
	}

	err := client.UploadBlob(t.Context(), newDigest(payload), bytes.NewReader(payload))
	require.Equal(t, codes.DataLoss, status.Code(err))
}

type readByteStream struct {
	bytestream.ByteStreamClient
	stream  bytestream.ByteStream_ReadClient
	request *bytestream.ReadRequest
}

func (s *readByteStream) Read(_ context.Context, request *bytestream.ReadRequest, _ ...grpc.CallOption) (bytestream.ByteStream_ReadClient, error) {
	s.request = request
	return s.stream, nil
}

type scriptedReadStream struct {
	grpc.ClientStream
	responses []*bytestream.ReadResponse
}

func (s *scriptedReadStream) Recv() (*bytestream.ReadResponse, error) {
	if len(s.responses) == 0 {
		return nil, io.EOF
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return response, nil
}

func TestReadBlobVerifiesChunkedByteStreamDownload(t *testing.T) {
	payload := []byte("a Bonanza output blob")
	stream := &scriptedReadStream{
		responses: []*bytestream.ReadResponse{
			{Data: payload[:5]},
			{},
			{Data: payload[5:]},
		},
	}
	byteStream := &readByteStream{stream: stream}
	client := &client{
		instanceName: "c1",
		byteStream:   byteStream,
	}
	digest := newDigest(payload)

	contents, err := client.ReadBlob(t.Context(), digest)
	require.NoError(t, err)
	require.Equal(t, payload, contents)
	require.Equal(t, fmt.Sprintf("c1/blobs/%s/%d", digest.Hash, digest.SizeBytes), byteStream.request.ResourceName)
}

func TestReadBlobRejectsTruncatedByteStreamDownload(t *testing.T) {
	payload := []byte("a Bonanza output blob")
	byteStream := &readByteStream{
		stream: &scriptedReadStream{
			responses: []*bytestream.ReadResponse{
				{Data: payload[:len(payload)-1]},
			},
		},
	}
	client := &client{byteStream: byteStream}

	_, err := client.ReadBlob(t.Context(), newDigest(payload))
	require.Equal(t, codes.DataLoss, status.Code(err))
}

func TestTerminalOperationErrorRejectsOKStatus(t *testing.T) {
	err := terminalOperationError(int32(codes.OK), "malformed terminal operation")
	require.Equal(t, codes.Internal, status.Code(err))
}

type fakeExecutionService struct {
	remoteexecution.ExecutionClient
	executeStream grpc.ServerStreamingClient[longrunningpb.Operation]
	waitStream    grpc.ServerStreamingClient[longrunningpb.Operation]
	waitErr       error
	waitCount     int
	executeCount  int
	waitName      string
}

func (s *fakeExecutionService) Execute(_ context.Context, _ *remoteexecution.ExecuteRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[longrunningpb.Operation], error) {
	s.executeCount++
	return s.executeStream, nil
}

func (s *fakeExecutionService) WaitExecution(_ context.Context, request *remoteexecution.WaitExecutionRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[longrunningpb.Operation], error) {
	s.waitCount++
	s.waitName = request.Name
	return s.waitStream, s.waitErr
}

type scriptedOperationStream struct {
	grpc.ClientStream
	operations []*longrunningpb.Operation
	terminal   error
}

func (s *scriptedOperationStream) Recv() (*longrunningpb.Operation, error) {
	if len(s.operations) == 0 {
		return nil, s.terminal
	}
	operation := s.operations[0]
	s.operations = s.operations[1:]
	return operation, nil
}

type fakeOperationsService struct {
	longrunningpb.OperationsClient
	canceledName string
	contextErr   error
}

func (s *fakeOperationsService) CancelOperation(ctx context.Context, request *longrunningpb.CancelOperationRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	s.canceledName = request.Name
	s.contextErr = ctx.Err()
	return &emptypb.Empty{}, nil
}

func TestExecuteResumesNamedOperationWithoutDuplicateExecution(t *testing.T) {
	response := &remoteexecution.ExecuteResponse{
		Result:       &remoteexecution.ActionResult{ExitCode: 0},
		CachedResult: true,
	}
	packed, err := anypb.New(response)
	require.NoError(t, err)
	execution := &fakeExecutionService{
		executeStream: &scriptedOperationStream{
			operations: []*longrunningpb.Operation{{Name: "operations/one"}},
			terminal:   status.Error(codes.Unavailable, "stream disconnected"),
		},
		waitStream: &scriptedOperationStream{
			operations: []*longrunningpb.Operation{{Name: "operations/one", Done: true, Result: &longrunningpb.Operation_Response{Response: packed}}},
		},
	}
	c := &client{execution: execution}
	got, err := c.Execute(t.Context(), &remoteexecution.ExecuteRequest{})
	require.NoError(t, err)
	require.True(t, got.CachedResult)
	require.Zero(t, got.Result.ExitCode)
	require.Equal(t, 1, execution.executeCount)
	require.Equal(t, "operations/one", execution.waitName)
}

func TestExecuteCancellationCancelsNamedRemoteOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	operations := &fakeOperationsService{}
	execution := &fakeExecutionService{executeStream: &cancellationStream{
		scriptedOperationStream: scriptedOperationStream{
			operations: []*longrunningpb.Operation{{Name: "operations/canceled"}},
		},
		cancel: cancel,
	}}
	c := &client{execution: execution, operations: operations}
	_, err := c.Execute(ctx, &remoteexecution.ExecuteRequest{})
	require.Equal(t, codes.Canceled, status.Code(err))
	require.Equal(t, "operations/canceled", operations.canceledName)
	require.NoError(t, operations.contextErr)
	require.Empty(t, execution.waitName)
}

type cancellationStream struct {
	scriptedOperationStream
	cancel context.CancelFunc
}

func (s *cancellationStream) Recv() (*longrunningpb.Operation, error) {
	if len(s.operations) > 0 {
		return s.scriptedOperationStream.Recv()
	}
	s.cancel()
	return nil, status.Error(codes.Canceled, "execution canceled")
}

func TestExecuteDoesNotResubmitUnknownOperation(t *testing.T) {
	execution := &fakeExecutionService{executeStream: &scriptedOperationStream{
		terminal: status.Error(codes.Unavailable, "remote executor at capacity"),
	}}
	c := &client{execution: execution}
	_, err := c.Execute(t.Context(), &remoteexecution.ExecuteRequest{})
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Equal(t, 1, execution.executeCount)
	require.Empty(t, execution.waitName)
}

func TestExecuteStopsWaitingWhenOperationDisappears(t *testing.T) {
	execution := &fakeExecutionService{
		executeStream: &scriptedOperationStream{
			operations: []*longrunningpb.Operation{{Name: "operations/evicted"}},
			terminal:   io.EOF,
		},
		waitErr: status.Error(codes.NotFound, "operation evicted"),
	}
	_, err := (&client{execution: execution}).Execute(t.Context(), &remoteexecution.ExecuteRequest{})
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Equal(t, 1, execution.executeCount)
	require.Equal(t, 1, execution.waitCount)
}

func TestExecuteBoundsReconnectsUnderUnavailableBackend(t *testing.T) {
	execution := &fakeExecutionService{
		executeStream: &scriptedOperationStream{
			operations: []*longrunningpb.Operation{{Name: "operations/busy"}},
			terminal:   status.Error(codes.Unavailable, "backend overloaded"),
		},
		waitErr: status.Error(codes.Unavailable, "backend overloaded"),
	}
	_, err := (&client{execution: execution}).Execute(t.Context(), &remoteexecution.ExecuteRequest{})
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Equal(t, 1, execution.executeCount)
	require.Equal(t, 3, execution.waitCount)
}

func TestReadBlobRejectsOversizedTreeBeforeDownloading(t *testing.T) {
	c := &client{}
	_, err := c.ReadBlob(t.Context(), &remoteexecution.Digest{
		Hash:      newDigest(nil).Hash,
		SizeBytes: (64 << 20) + 1,
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
}

type fakeCapabilitiesService struct {
	remoteexecution.CapabilitiesClient
	request  *remoteexecution.GetCapabilitiesRequest
	response *remoteexecution.ServerCapabilities
}

func (s *fakeCapabilitiesService) GetCapabilities(_ context.Context, request *remoteexecution.GetCapabilitiesRequest, _ ...grpc.CallOption) (*remoteexecution.ServerCapabilities, error) {
	s.request = request
	return s.response, nil
}

type readinessCAS struct {
	remoteexecution.ContentAddressableStorageClient
	request *remoteexecution.FindMissingBlobsRequest
	err     error
}

func (s *readinessCAS) FindMissingBlobs(_ context.Context, request *remoteexecution.FindMissingBlobsRequest, _ ...grpc.CallOption) (*remoteexecution.FindMissingBlobsResponse, error) {
	s.request = request
	if s.err != nil {
		return nil, s.err
	}
	return &remoteexecution.FindMissingBlobsResponse{}, nil
}

func TestReadinessRequiresSameInstanceExecutionAndCAS(t *testing.T) {
	capabilities := &fakeCapabilitiesService{response: &remoteexecution.ServerCapabilities{
		ExecutionCapabilities: &remoteexecution.ExecutionCapabilities{
			ExecEnabled:     true,
			DigestFunctions: []remoteexecution.DigestFunction_Value{remoteexecution.DigestFunction_SHA256},
		},
		CacheCapabilities: &remoteexecution.CacheCapabilities{
			DigestFunctions: []remoteexecution.DigestFunction_Value{remoteexecution.DigestFunction_SHA256},
		},
	}}
	cas := &readinessCAS{}
	c := &client{instanceName: "c1", capabilities: capabilities, cas: cas}
	require.NoError(t, c.CheckReadiness(t.Context()))
	require.Equal(t, "c1", capabilities.request.InstanceName)
	require.Equal(t, "c1", cas.request.InstanceName)

	cas.err = status.Error(codes.ResourceExhausted, "CAS overloaded")
	require.Equal(t, codes.ResourceExhausted, status.Code(c.CheckReadiness(t.Context())))
	cas.err = nil
	capabilities.response.ExecutionCapabilities.ExecEnabled = false
	require.Equal(t, codes.FailedPrecondition, status.Code(c.CheckReadiness(t.Context())))
}

func TestExecuteRejectsMalformedTerminalAndPreservesFailureCode(t *testing.T) {
	execution := &fakeExecutionService{executeStream: &scriptedOperationStream{
		operations: []*longrunningpb.Operation{{
			Name: "operations/failed", Done: true,
			Result: &longrunningpb.Operation_Error{Error: status.New(codes.DeadlineExceeded, "worker timeout").Proto()},
		}},
	}}
	_, err := (&client{execution: execution}).Execute(t.Context(), &remoteexecution.ExecuteRequest{})
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
	require.ErrorContains(t, err, "worker timeout")
}

// Exercise the real NewClient wiring over one gRPC connection, not just the
// individual service stubs above. A misrouted CAS endpoint makes readiness or
// cache lookup fail before Execute can report a successful action.
type localREAPIServer struct {
	remoteexecution.UnimplementedCapabilitiesServer
	remoteexecution.UnimplementedContentAddressableStorageServer
	remoteexecution.UnimplementedExecutionServer
	capabilitiesInstance string
	casInstance          string
	executionInstance    string
}

func (s *localREAPIServer) GetCapabilities(_ context.Context, request *remoteexecution.GetCapabilitiesRequest) (*remoteexecution.ServerCapabilities, error) {
	s.capabilitiesInstance = request.InstanceName
	return &remoteexecution.ServerCapabilities{
		ExecutionCapabilities: &remoteexecution.ExecutionCapabilities{
			ExecEnabled:     true,
			DigestFunctions: []remoteexecution.DigestFunction_Value{remoteexecution.DigestFunction_SHA256},
		},
		CacheCapabilities: &remoteexecution.CacheCapabilities{
			DigestFunctions: []remoteexecution.DigestFunction_Value{remoteexecution.DigestFunction_SHA256},
		},
	}, nil
}

func (s *localREAPIServer) FindMissingBlobs(_ context.Context, request *remoteexecution.FindMissingBlobsRequest) (*remoteexecution.FindMissingBlobsResponse, error) {
	s.casInstance = request.InstanceName
	return &remoteexecution.FindMissingBlobsResponse{}, nil
}

func (s *localREAPIServer) Execute(request *remoteexecution.ExecuteRequest, stream grpc.ServerStreamingServer[longrunningpb.Operation]) error {
	s.executionInstance = request.InstanceName
	response, err := anypb.New(&remoteexecution.ExecuteResponse{
		Result:       &remoteexecution.ActionResult{ExitCode: 0},
		CachedResult: true,
	})
	if err != nil {
		return err
	}
	return stream.Send(&longrunningpb.Operation{
		Name:   "operations/cached",
		Done:   true,
		Result: &longrunningpb.Operation_Response{Response: response},
	})
}

func TestClientUsesOneInstanceForExecutionAndCAS(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	service := &localREAPIServer{}
	remoteexecution.RegisterCapabilitiesServer(server, service)
	remoteexecution.RegisterContentAddressableStorageServer(server, service)
	remoteexecution.RegisterExecutionServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	connection, err := grpc.NewClient(
		"passthrough:///reapi-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	client := NewClient(connection, "c1/nonprod")
	require.NoError(t, client.CheckReadiness(t.Context()))
	// A present CAS blob is a cache hit; no ByteStream upload is necessary.
	require.NoError(t, client.UploadBlob(t.Context(), newDigest(nil), bytes.NewReader(nil)))
	response, err := client.Execute(t.Context(), &remoteexecution.ExecuteRequest{
		InstanceName:   "c1/nonprod",
		ActionDigest:   newDigest(nil),
		DigestFunction: remoteexecution.DigestFunction_SHA256,
	})
	require.NoError(t, err)
	require.True(t, response.CachedResult)
	require.Equal(t, "c1/nonprod", service.capabilitiesInstance)
	require.Equal(t, "c1/nonprod", service.casInstance)
	require.Equal(t, "c1/nonprod", service.executionInstance)
}
