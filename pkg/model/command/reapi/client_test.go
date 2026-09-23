package reapi

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
)

type acceptingCAS struct {
	remoteexecution.ContentAddressableStorageClient
}

func (acceptingCAS) FindMissingBlobs(_ context.Context, request *remoteexecution.FindMissingBlobsRequest, _ ...grpc.CallOption) (*remoteexecution.FindMissingBlobsResponse, error) {
	return &remoteexecution.FindMissingBlobsResponse{MissingBlobDigests: request.BlobDigests}, nil
}

type recordingByteStream struct {
	bytestream.ByteStreamClient
	write *recordingWriteStream
}

func (s recordingByteStream) Write(context.Context, ...grpc.CallOption) (bytestream.ByteStream_WriteClient, error) {
	return s.write, nil
}

type recordingWriteStream struct {
	grpc.ClientStream
	data         bytes.Buffer
	finished     bool
	finishOffset int64
}

func (s *recordingWriteStream) Send(request *bytestream.WriteRequest) error {
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

func TestUploadBlobFinalizesAfterReaderReturnsEOFSeparately(t *testing.T) {
	payload := []byte("a Bonanza input blob")
	stream := &recordingWriteStream{}
	client := &client{
		cas:        acceptingCAS{},
		byteStream: recordingByteStream{write: stream},
	}
	digest := newDigest(payload)
	require.NoError(t, client.UploadBlob(t.Context(), digest, bytes.NewReader(payload)))
	require.Equal(t, payload, stream.data.Bytes())
	require.Equal(t, int64(len(payload)), stream.finishOffset)
}
