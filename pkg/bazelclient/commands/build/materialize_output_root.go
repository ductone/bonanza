package build

import (
	"context"
	"fmt"
	"os"

	model_core "bonanza.build/pkg/model/core"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_parser "bonanza.build/pkg/model/parser"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
)

// outputRootMaterializer writes a directory hierarchy that is stored in
// object storage to a directory on the local system.
type outputRootMaterializer struct {
	context                 context.Context
	directoryContentsReader model_parser.MessageObjectReader[object.LocalReference, *model_filesystem_pb.DirectoryContents]
	leavesReader            model_parser.MessageObjectReader[object.LocalReference, *model_filesystem_pb.Leaves]
	fileReader              *model_filesystem.FileReader[object.LocalReference]

	filesWritten    int
	symlinksWritten int
}

// materializeDirectory writes the contents of a single directory, and
// recursively descends into any directories it contains.
//
// Entries that already exist are removed before being recreated, as the
// permissions of an existing entry cannot be adjusted through the
// filesystem.Directory interface.
func (m *outputRootMaterializer) materializeDirectory(contents model_core.Message[*model_filesystem_pb.DirectoryContents, object.LocalReference], d filesystem.Directory) error {
	if contents.Message.GetLeaves() != nil {
		leaves, err := model_filesystem.DirectoryGetLeaves(m.context, m.leavesReader, contents)
		if err != nil {
			return err
		}

		for _, entry := range leaves.Message.Files {
			name, ok := path.NewComponent(entry.Name)
			if !ok {
				return fmt.Errorf("invalid file name %#v", entry.Name)
			}
			if err := m.materializeFile(model_core.Nested(leaves, entry.Properties), d, name); err != nil {
				return fmt.Errorf("file %#v: %w", entry.Name, err)
			}
		}

		for _, entry := range leaves.Message.Symlinks {
			name, ok := path.NewComponent(entry.Name)
			if !ok {
				return fmt.Errorf("invalid symbolic link name %#v", entry.Name)
			}
			d.RemoveAll(name)
			if err := d.Symlink(path.UNIXFormat.NewParser(entry.Target), name); err != nil {
				return fmt.Errorf("symbolic link %#v: %w", entry.Name, err)
			}
			m.symlinksWritten++
		}
	}

	for _, entry := range contents.Message.Directories {
		name, ok := path.NewComponent(entry.Name)
		if !ok {
			return fmt.Errorf("invalid directory name %#v", entry.Name)
		}
		childContents, err := model_filesystem.DirectoryGetContents(
			m.context,
			m.directoryContentsReader,
			model_core.Nested(contents, entry.Directory),
		)
		if err != nil {
			return fmt.Errorf("directory %#v: %w", entry.Name, err)
		}
		if err := d.Mkdir(name, 0o777); err != nil && !os.IsExist(err) {
			return fmt.Errorf("directory %#v: %w", entry.Name, err)
		}
		child, err := d.EnterDirectory(name)
		if err != nil {
			return fmt.Errorf("directory %#v: %w", entry.Name, err)
		}
		err = m.materializeDirectory(childContents, child)
		child.Close()
		if err != nil {
			return fmt.Errorf("directory %#v: %w", entry.Name, err)
		}
	}
	return nil
}

func (m *outputRootMaterializer) materializeFile(properties model_core.Message[*model_filesystem_pb.FileProperties, object.LocalReference], d filesystem.Directory, name path.Component) error {
	perm := os.FileMode(0o644)
	if properties.Message.GetIsExecutable() {
		perm = 0o755
	}
	d.RemoveAll(name)
	w, err := d.OpenWrite(name, filesystem.CreateExcl(perm))
	if err != nil {
		return err
	}
	defer w.Close()

	m.filesWritten++
	if properties.Message.GetContents() == nil {
		// Empty files are stored without any contents.
		return nil
	}
	fileContents, err := model_filesystem.NewFileContentsEntryFromProto(
		model_core.Nested(properties, properties.Message.Contents),
	)
	if err != nil {
		return err
	}
	return m.fileReader.FileWriteTo(m.context, fileContents, w)
}
