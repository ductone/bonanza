package buffered_test

import (
	"context"
	"testing"

	"bonanza.build/pkg/model/core/buffered"
	"bonanza.build/pkg/storage/dag"
	"bonanza.build/pkg/storage/object"

	"github.com/stretchr/testify/require"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type uploadResultDAGUploader struct {
	dag.Uploader[struct{}, object.LocalReference]
	err error
}

func (u uploadResultDAGUploader) UploadDAG(context.Context, object.LocalReference, dag.ObjectContentsWalker) error {
	return u.err
}

func TestObjectExporterUploadFailure(t *testing.T) {
	reference := object.MustNewSHA256V1LocalReference(
		"ca3d70c749a21a8a6bd60ef74b7fc988e149881e48fde520abe665d6a5995344",
		85845, 7, 1, 599552,
	)
	internalReference := buffered.Reference{LocalReference: reference}

	t.Run("StorageUnavailable", func(t *testing.T) {
		exporter := buffered.NewObjectExporter(uploadResultDAGUploader{
			err: status.Error(codes.Unavailable, "storage unavailable"),
		})
		exported, err := exporter.ExportReference(context.Background(), internalReference)
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.Equal(t, object.LocalReference{}, exported)
	})

	t.Run("Canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		exporter := buffered.NewObjectExporter(uploadResultDAGUploader{err: ctx.Err()})
		exported, err := exporter.ExportReference(ctx, internalReference)
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, object.LocalReference{}, exported)
	})

	t.Run("Uploaded", func(t *testing.T) {
		exporter := buffered.NewObjectExporter(uploadResultDAGUploader{})
		exported, err := exporter.ExportReference(context.Background(), internalReference)
		require.NoError(t, err)
		require.Equal(t, reference, exported)
	})
}
