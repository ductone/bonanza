package reapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
