// Package reapi bridges Bonanza command actions to a Remote Execution v2 backend.
package reapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	transferChunkSize   = 64 << 10
	resourceNameHeader  = "build.bazel.remote.execution.v2.resource-name"
	maxBufferedBlobSize = 64 << 20 // Trees are decoded in memory; remote digests must not cause unbounded allocation.
	maxWaitReconnects   = 3
)

// Client is the REv2 boundary used by Executor. It deliberately accepts only
// REv2 messages and CAS blobs; encrypted Bonanza scheduler actions never cross
// this boundary.
type Client interface {
	CheckReadiness(ctx context.Context) error
	UploadBlob(ctx context.Context, digest *remoteexecution.Digest, contents io.Reader) error
	OpenBlob(ctx context.Context, digest *remoteexecution.Digest) (io.ReadCloser, error)
	ReadBlob(ctx context.Context, digest *remoteexecution.Digest) ([]byte, error)
	Execute(ctx context.Context, request *remoteexecution.ExecuteRequest) (*remoteexecution.ExecuteResponse, error)
}

type client struct {
	instanceName string
	execution    remoteexecution.ExecutionClient
	operations   longrunningpb.OperationsClient
	capabilities remoteexecution.CapabilitiesClient
	cas          remoteexecution.ContentAddressableStorageClient
	byteStream   bytestream.ByteStreamClient
}

// NewClient creates an REv2 client over a configured Buildbarn connection.
func NewClient(connection grpc.ClientConnInterface, instanceName string) Client {
	return &client{
		instanceName: instanceName,
		execution:    remoteexecution.NewExecutionClient(connection),
		operations:   longrunningpb.NewOperationsClient(connection),
		capabilities: remoteexecution.NewCapabilitiesClient(connection),
		cas:          remoteexecution.NewContentAddressableStorageClient(connection),
		byteStream:   bytestream.NewByteStreamClient(connection),
	}
}

func (c *client) CheckReadiness(ctx context.Context) error {
	capabilities, err := c.capabilities.GetCapabilities(ctx, &remoteexecution.GetCapabilitiesRequest{
		InstanceName: c.instanceName,
	})
	if err != nil {
		return fmt.Errorf("check REAPI execution capabilities: %w", err)
	}
	execution := capabilities.GetExecutionCapabilities()
	if !execution.GetExecEnabled() || !supportsSHA256(execution.GetDigestFunctions(), execution.GetDigestFunction()) ||
		!supportsSHA256(capabilities.GetCacheCapabilities().GetDigestFunctions(), remoteexecution.DigestFunction_UNKNOWN) {
		return status.Error(codes.FailedPrecondition, "REAPI endpoint does not expose SHA-256 execution and CAS for this instance")
	}
	emptyDigest := newDigest(nil)
	missing, err := c.cas.FindMissingBlobs(ctx, &remoteexecution.FindMissingBlobsRequest{
		InstanceName:   c.instanceName,
		BlobDigests:    []*remoteexecution.Digest{emptyDigest},
		DigestFunction: remoteexecution.DigestFunction_SHA256,
	})
	if err != nil {
		return fmt.Errorf("check REAPI CAS readiness: %w", err)
	}
	if missing == nil {
		return status.Error(codes.DataLoss, "REAPI CAS returned no readiness response")
	}
	return nil
}

func supportsSHA256(functions []remoteexecution.DigestFunction_Value, legacy remoteexecution.DigestFunction_Value) bool {
	if len(functions) == 0 {
		return legacy == remoteexecution.DigestFunction_SHA256
	}
	for _, function := range functions {
		if function == remoteexecution.DigestFunction_SHA256 {
			return true
		}
	}
	return false
}

func (c *client) UploadBlob(ctx context.Context, digest *remoteexecution.Digest, contents io.Reader) error {
	if err := validateDigest(digest); err != nil {
		return err
	}
	if contents == nil {
		return status.Error(codes.InvalidArgument, "no contents provided for REAPI blob upload")
	}

	missing, err := c.cas.FindMissingBlobs(ctx, &remoteexecution.FindMissingBlobsRequest{
		InstanceName:   c.instanceName,
		BlobDigests:    []*remoteexecution.Digest{digest},
		DigestFunction: remoteexecution.DigestFunction_SHA256,
	})
	if err != nil {
		return fmt.Errorf("find missing REAPI blob %s/%d: %w", digest.Hash, digest.SizeBytes, err)
	}
	if missing == nil {
		return status.Error(codes.DataLoss, "REAPI CAS returned no FindMissingBlobs response")
	}
	if !containsDigest(missing.MissingBlobDigests, digest) {
		return nil
	}

	resourceName := c.writeResourceName(digest)
	uploadContext, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.byteStream.Write(metadata.AppendToOutgoingContext(uploadContext, resourceNameHeader, resourceName))
	if err != nil {
		return fmt.Errorf("open ByteStream upload for REAPI blob %s/%d: %w", digest.Hash, digest.SizeBytes, err)
	}
	buffer := make([]byte, transferChunkSize)
	hasher := sha256.New()
	var written int64
	firstRequest := true
	for {
		n, readErr := contents.Read(buffer)
		if n > 0 {
			data := buffer[:n]
			if _, err := hasher.Write(data); err != nil {
				return c.closeUploadWithError(stream, cancel, err)
			}
			request := &bytestream.WriteRequest{
				WriteOffset: written,
				Data:        data,
			}
			if firstRequest {
				request.ResourceName = resourceName
				firstRequest = false
			}
			written += int64(n)
			if err := stream.Send(request); err != nil {
				return c.closeUploadAfterSendError(stream, cancel, digest, fmt.Errorf("write REAPI blob %s/%d: %w", digest.Hash, digest.SizeBytes, err))
			}
		}
		if readErr == io.EOF {
			if written != digest.SizeBytes {
				return c.closeUploadWithError(stream, cancel, status.Errorf(codes.InvalidArgument, "REAPI blob source has %d bytes, expected %d", written, digest.SizeBytes))
			}
			if actualHash := hex.EncodeToString(hasher.Sum(nil)); actualHash != digest.Hash {
				return c.closeUploadWithError(stream, cancel, status.Errorf(codes.InvalidArgument, "REAPI blob source has SHA-256 %s, expected %s", actualHash, digest.Hash))
			}
			finish := &bytestream.WriteRequest{WriteOffset: written, FinishWrite: true}
			if firstRequest {
				finish.ResourceName = resourceName
			}
			if err := stream.Send(finish); err != nil {
				return c.closeUploadAfterSendError(stream, cancel, digest, fmt.Errorf("finish REAPI blob upload %s/%d: %w", digest.Hash, digest.SizeBytes, err))
			}
			response, err := stream.CloseAndRecv()
			if err != nil {
				return fmt.Errorf("finish REAPI blob upload %s/%d: %w", digest.Hash, digest.SizeBytes, err)
			}
			if response == nil || response.CommittedSize != digest.SizeBytes {
				return status.Errorf(codes.DataLoss, "REAPI server committed an unexpected size for blob %s/%d", digest.Hash, digest.SizeBytes)
			}
			return nil
		}
		if readErr != nil {
			return c.closeUploadWithError(stream, cancel, fmt.Errorf("read REAPI blob source %s/%d: %w", digest.Hash, digest.SizeBytes, readErr))
		}
	}
}

func (c *client) OpenBlob(ctx context.Context, digest *remoteexecution.Digest) (io.ReadCloser, error) {
	if err := validateDigest(digest); err != nil {
		return nil, err
	}
	resourceName := c.readResourceName(digest)
	readContext, cancel := context.WithCancel(ctx)
	stream, err := c.byteStream.Read(
		metadata.AppendToOutgoingContext(readContext, resourceNameHeader, resourceName),
		&bytestream.ReadRequest{ResourceName: resourceName},
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open ByteStream download for REAPI blob %s/%d: %w", digest.Hash, digest.SizeBytes, err)
	}
	return &byteStreamBlobReader{
		stream: stream,
		cancel: cancel,
		digest: digest,
		hasher: sha256.New(),
	}, nil
}

func (c *client) ReadBlob(ctx context.Context, digest *remoteexecution.Digest) ([]byte, error) {
	if err := validateDigest(digest); err != nil {
		return nil, err
	}
	if digest.SizeBytes > maxBufferedBlobSize {
		return nil, status.Errorf(codes.ResourceExhausted, "REAPI blob %s exceeds the %d-byte in-memory import limit", digest.Hash, maxBufferedBlobSize)
	}
	reader, err := c.OpenBlob(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	contents, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read REAPI blob %s/%d: %w", digest.Hash, digest.SizeBytes, err)
	}
	return contents, nil
}

type byteStreamBlobReader struct {
	stream    bytestream.ByteStream_ReadClient
	cancel    context.CancelFunc
	digest    *remoteexecution.Digest
	hasher    hash.Hash
	chunk     []byte
	readBytes int64
	done      bool
}

func (r *byteStreamBlobReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.chunk) == 0 {
		if r.done {
			return 0, io.EOF
		}
		response, err := r.stream.Recv()
		if err == io.EOF {
			r.done = true
			r.cancel()
			if r.readBytes != r.digest.SizeBytes {
				return 0, status.Errorf(codes.DataLoss, "REAPI blob %s has %d bytes, expected %d", r.digest.Hash, r.readBytes, r.digest.SizeBytes)
			}
			if actualHash := hex.EncodeToString(r.hasher.Sum(nil)); actualHash != r.digest.Hash {
				return 0, status.Errorf(codes.DataLoss, "REAPI blob has SHA-256 %s, expected %s", actualHash, r.digest.Hash)
			}
			return 0, io.EOF
		}
		if err != nil {
			r.done = true
			r.cancel()
			return 0, err
		}
		r.chunk = response.Data
	}
	n := copy(p, r.chunk)
	r.chunk = r.chunk[n:]
	if _, err := r.hasher.Write(p[:n]); err != nil {
		return 0, err
	}
	r.readBytes += int64(n)
	if r.readBytes > r.digest.SizeBytes {
		r.done = true
		r.cancel()
		return 0, status.Errorf(codes.DataLoss, "REAPI blob %s exceeds advertised size of %d bytes", r.digest.Hash, r.digest.SizeBytes)
	}
	return n, nil
}

func (r *byteStreamBlobReader) Close() error {
	if !r.done {
		r.done = true
		r.cancel()
	}
	return nil
}

func (c *client) Execute(ctx context.Context, request *remoteexecution.ExecuteRequest) (*remoteexecution.ExecuteResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "no REAPI Execute request provided")
	}
	stream, err := c.execution.Execute(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("start REAPI execution: %w", err)
	}
	var operationName string
	reconnects := 0
	for {
		operation, recvErr := stream.Recv()
		if recvErr != nil {
			if ctx.Err() != nil {
				c.cancelOperation(ctx, operationName)
				return nil, executionFailureStatus(ctx.Err())
			}
			// A named operation can be resumed without submitting the action a
			// second time. Never retry Execute after an ambiguous stream failure.
			if operationName != "" && reconnects < maxWaitReconnects &&
				(errors.Is(recvErr, io.EOF) || status.Code(recvErr) == codes.Unavailable) {
				resumed := false
				for reconnects < maxWaitReconnects {
					reconnects++
					timer := time.NewTimer(time.Duration(reconnects) * 100 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						c.cancelOperation(ctx, operationName)
						return nil, executionFailureStatus(ctx.Err())
					case <-timer.C:
					}
					stream, err = c.execution.WaitExecution(ctx, &remoteexecution.WaitExecutionRequest{Name: operationName})
					if err == nil {
						resumed = true
						break
					}
					recvErr = err
					if ctx.Err() != nil {
						c.cancelOperation(ctx, operationName)
						return nil, executionFailureStatus(ctx.Err())
					}
					if status.Code(err) != codes.Unavailable {
						break
					}
				}
				if resumed {
					continue
				}
			}
			if errors.Is(recvErr, io.EOF) {
				return nil, status.Error(codes.Internal, "REAPI Execute stream ended without a completed operation")
			}
			return nil, fmt.Errorf("receive REAPI execution update: %w", recvErr)
		}
		if operation == nil {
			return nil, status.Error(codes.DataLoss, "REAPI execution returned a nil operation")
		}
		if operationName != "" && operation.GetName() != "" && operation.GetName() != operationName {
			return nil, status.Errorf(codes.DataLoss, "REAPI execution changed operation name from %q to %q", operationName, operation.GetName())
		}
		if operation.GetName() != "" {
			operationName = operation.GetName()
		}
		if !operation.GetDone() {
			continue
		}
		if operationError := operation.GetError(); operationError != nil {
			return nil, terminalOperationError(operationError.Code, operationError.Message)
		}
		responseAny := operation.GetResponse()
		if responseAny == nil {
			return nil, status.Error(codes.Internal, "REAPI completed operation has no response")
		}
		var response remoteexecution.ExecuteResponse
		if err := responseAny.UnmarshalTo(&response); err != nil {
			return nil, fmt.Errorf("unmarshal REAPI Execute response: %w", err)
		}
		return &response, nil
	}
}

// CancelOperation is best effort and may be unimplemented by a REAPI server.
// Use the same authenticated connection, but a fresh bounded context: the
// execution context is already canceled when cleanup becomes necessary.
func (c *client) cancelOperation(ctx context.Context, name string) {
	if name == "" {
		return
	}
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, _ = c.operations.CancelOperation(cancelCtx, &longrunningpb.CancelOperationRequest{Name: name})
}

func (c *client) readResourceName(digest *remoteexecution.Digest) string {
	return c.resourceName("blobs", digest)
}

func (c *client) writeResourceName(digest *remoteexecution.Digest) string {
	return c.resourceName("uploads/"+uuid.NewString()+"/blobs", digest)
}

func (c *client) resourceName(prefix string, digest *remoteexecution.Digest) string {
	parts := make([]string, 0, 4)
	if c.instanceName != "" {
		parts = append(parts, c.instanceName)
	}
	parts = append(parts, prefix, digest.Hash, fmt.Sprintf("%d", digest.SizeBytes))
	return strings.Join(parts, "/")
}

func (client) closeUploadWithError(stream bytestream.ByteStream_WriteClient, cancel context.CancelFunc, cause error) error {
	cancel()
	_, _ = stream.CloseAndRecv()
	return cause
}

// REAPI permits a concurrent uploader to complete the same blob mid-stream.
// A full committed size proves the requested blob is already present.
func (client) closeUploadAfterSendError(
	stream bytestream.ByteStream_WriteClient,
	cancel context.CancelFunc,
	digest *remoteexecution.Digest,
	cause error,
) error {
	response, err := stream.CloseAndRecv()
	cancel()
	if err != nil {
		return fmt.Errorf("confirm REAPI blob upload %s/%d after send failure (%v): %w", digest.Hash, digest.SizeBytes, cause, err)
	}
	if response == nil || response.CommittedSize != digest.SizeBytes {
		return status.Errorf(codes.DataLoss, "REAPI server committed an unexpected size for blob %s/%d", digest.Hash, digest.SizeBytes)
	}
	return nil
}

// An Operation error status must be non-OK. status.Error would turn an OK
// status into nil, silently accepting a malformed terminal operation.
func terminalOperationError(code int32, message string) error {
	if code == int32(codes.OK) {
		return status.Error(codes.Internal, "REAPI completed operation has an OK error status")
	}
	return status.Error(codes.Code(code), message)
}

func containsDigest(digests []*remoteexecution.Digest, expected *remoteexecution.Digest) bool {
	for _, digest := range digests {
		if digest.GetHash() == expected.Hash && digest.GetSizeBytes() == expected.SizeBytes {
			return true
		}
	}
	return false
}

func validateDigest(digest *remoteexecution.Digest) error {
	if digest == nil {
		return status.Error(codes.InvalidArgument, "no REAPI digest provided")
	}
	if digest.SizeBytes < 0 {
		return status.Errorf(codes.InvalidArgument, "REAPI digest %s has a negative size", digest.Hash)
	}
	if len(digest.Hash) != sha256.Size*2 {
		return status.Errorf(codes.InvalidArgument, "REAPI digest %q is not a SHA-256 hash", digest.Hash)
	}
	if _, err := hex.DecodeString(digest.Hash); err != nil {
		return status.Errorf(codes.InvalidArgument, "REAPI digest %q is not hexadecimal", digest.Hash)
	}
	if digest.Hash != strings.ToLower(digest.Hash) {
		return status.Errorf(codes.InvalidArgument, "REAPI digest %q is not lowercase", digest.Hash)
	}
	return nil
}
