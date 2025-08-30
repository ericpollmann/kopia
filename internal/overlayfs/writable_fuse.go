//go:build !windows && !openbsd && !freebsd
// +build !windows,!openbsd,!freebsd

// Package overlayfs implements writable FUSE nodes for overlay filesystem.
package overlayfs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	gofusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
)

// WritableDirectoryNode implements a writable FUSE directory node backed by overlay filesystem.
type WritableDirectoryNode struct {
	gofusefs.Inode
	entry fs.Directory
	ofs   *OverlayFS
	path  string
}

// WritableFileNode implements a writable FUSE file node backed by overlay filesystem.
type WritableFileNode struct {
	gofusefs.Inode
	entry fs.File
	ofs   *OverlayFS
	path  string
}

// WritableFileHandle implements a writable file handle for overlay files.
type WritableFileHandle struct {
	file     *os.File
	ofs      *OverlayFS
	path     string
	flags    uint32
	modified bool
}

// NewWritableDirectoryNode creates a new writable directory node.
func NewWritableDirectoryNode(entry fs.Directory, ofs *OverlayFS, path string) gofusefs.InodeEmbedder {
	return &WritableDirectoryNode{
		entry: entry,
		ofs:   ofs,
		path:  path,
	}
}

// NewWritableFileNode creates a new writable file node.
func NewWritableFileNode(entry fs.File, ofs *OverlayFS, path string) gofusefs.InodeEmbedder {
	return &WritableFileNode{
		entry: entry,
		ofs:   ofs,
		path:  path,
	}
}

// Getattr implements the NodeGetattrer interface.
func (n *WritableDirectoryNode) Getattr(_ context.Context, _ gofusefs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	populateAttributesFromEntry(&out.Attr, n.entry)
	// Make directory writable by setting owner write permission
	out.Attr.Mode |= 0o200 // Add write permission for owner
	out.Ino = n.StableAttr().Ino
	return gofusefs.OK
}

// Getattr implements the NodeGetattrer interface.
func (n *WritableFileNode) Getattr(_ context.Context, _ gofusefs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	populateAttributesFromEntry(&out.Attr, n.entry)
	// Make file writable
	out.Attr.Mode |= 0o200 // Add write permission for owner
	out.Ino = n.StableAttr().Ino
	return gofusefs.OK
}

// populateAttributesFromEntry populates FUSE attributes from fs.Entry.
func populateAttributesFromEntry(attr *fuse.Attr, entry fs.Entry) {
	attr.Mode = goModeToUnixMode(entry.Mode())
	attr.Size = uint64(entry.Size())            //nolint:gosec
	attr.Mtime = uint64(entry.ModTime().Unix()) //nolint:gosec
	attr.Ctime = attr.Mtime
	attr.Atime = attr.Mtime
	attr.Nlink = 1
	attr.Uid = entry.Owner().UserID
	attr.Gid = entry.Owner().GroupID
	attr.Blocks = (attr.Size + 4095) / 4096 //nolint:mnd // Standard block size
}

// goModeToUnixMode converts Go file mode to Unix mode.
func goModeToUnixMode(mode os.FileMode) uint32 {
	unixmode := uint32(mode.Perm())

	if mode&os.ModeSetuid != 0 {
		unixmode |= 0o4000
	}

	if mode&os.ModeSetgid != 0 {
		unixmode |= 0o2000
	}

	if mode&os.ModeSticky != 0 {
		unixmode |= 0o1000
	}

	return unixmode
}

// Lookup implements the NodeLookuper interface.
func (n *WritableDirectoryNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofusefs.Inode, syscall.Errno) {
	childPath := n.path
	if childPath == "" {
		childPath = name
	} else {
		childPath = childPath + "/" + name
	}

	entry, err := n.ofs.GetEntry(ctx, childPath)
	if err != nil {
		if os.IsNotExist(err) {
			log(ctx).Debugf("lookup not found (ENOENT): %v in %v", name, n.entry.Name())
			return nil, syscall.ENOENT
		}
		log(ctx).Errorf("lookup error %v in %v: %v (type: %T)", name, n.entry.Name(), err, err)
		return nil, syscall.EIO
	}

	if entry == nil {
		return nil, syscall.ENOENT
	}

	stable := gofusefs.StableAttr{
		Mode: entryToFuseMode(entry),
	}

	var node gofusefs.InodeEmbedder
	switch e := entry.(type) {
	case fs.Directory:
		node = NewWritableDirectoryNode(e, n.ofs, childPath)
	case fs.File:
		node = NewWritableFileNode(e, n.ofs, childPath)
	default:
		return nil, syscall.EIO
	}

	child := n.NewInode(ctx, node, stable)
	populateAttributesFromEntry(&out.Attr, entry)

	return child, gofusefs.OK
}

// Readdir implements NodeReaddirer interface for directory listing.
func (n *WritableDirectoryNode) Readdir(ctx context.Context) (gofusefs.DirStream, syscall.Errno) {
	var result []fuse.DirEntry

	iter, err := n.entry.Iterate(ctx)
	if err != nil {
		log(ctx).Errorf("error reading directory %v: %v", n.entry.Name(), err)
		return nil, syscall.EIO
	}
	defer iter.Close()

	for {
		entry, err := iter.Next(ctx)
		if entry == nil {
			break
		}
		if err != nil {
			log(ctx).Errorf("error iterating directory %v: %v", n.entry.Name(), err)
			return nil, syscall.EIO
		}

		result = append(result, fuse.DirEntry{
			Name: entry.Name(),
			Mode: entryToFuseMode(entry),
		})
	}

	return gofusefs.NewListDirStream(result), gofusefs.OK
}

// Access implements NodeAccesser interface for permission checking.
func (n *WritableDirectoryNode) Access(_ context.Context, mask uint32) syscall.Errno {
	// In writable mode, allow all access for owner
	// We could do more sophisticated checking here if needed
	return gofusefs.OK
}

// entryToFuseMode converts fs.Entry type to FUSE mode.
func entryToFuseMode(e fs.Entry) uint32 {
	// Check if entry implements fs.Directory interface (handles wrapped directories)
	if _, isDir := e.(fs.Directory); isDir {
		return fuse.S_IFDIR
	}
	// Check if entry implements fs.File interface
	if _, isFile := e.(fs.File); isFile {
		return fuse.S_IFREG
	}
	// Default to regular file
	return fuse.S_IFREG
}

// Create implements the NodeCreater interface.
func (n *WritableDirectoryNode) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*gofusefs.Inode, gofusefs.FileHandle, uint32, syscall.Errno) {
	childPath := n.path
	if childPath == "" {
		childPath = name
	} else {
		childPath = childPath + "/" + name
	}

	// Check file existence across all layers for proper flag handling
	existsInOverlay := false
	existsInSnapshot := false
	isDeleted := n.ofs.isFileDeleted(childPath)

	// Check overlay layer
	if _, err := n.ofs.getOverlayFile(childPath); err == nil {
		existsInOverlay = true
	}

	// Check snapshot layer (only if not deleted and not in overlay)
	if !existsInOverlay && !isDeleted {
		if _, err := n.ofs.findEntryInSnapshot(ctx, childPath); err == nil {
			existsInSnapshot = true
		}
	}

	// Handle O_EXCL flag - fail if file exists anywhere visible
	if flags&syscall.O_EXCL != 0 {
		fileExists := existsInOverlay || (existsInSnapshot && !isDeleted)
		if fileExists {
			log(ctx).Debugf("Create failed: file exists (O_EXCL) %v", name)
			return nil, nil, 0, syscall.EEXIST
		}
	}

	// If file exists in overlay and is deleted, remove the deleted marker
	if isDeleted {
		deletedPath := filepath.Join(n.ofs.deletedDir, childPath)
		if err := os.Remove(deletedPath); err != nil {
			log(ctx).Errorf("error removing delete marker %v: %v", name, err)
		}
	}

	// Create empty file in overlay with proper mode
	content := []byte{}
	if flags&syscall.O_TRUNC == 0 && existsInSnapshot && !existsInOverlay {
		// If not truncating and file exists in snapshot, copy it first
		if entry, err := n.ofs.findEntryInSnapshot(ctx, childPath); err == nil {
			if fileEntry, ok := entry.(fs.File); ok {
				if reader, err := fileEntry.Open(ctx); err == nil {
					if data, err := io.ReadAll(reader); err == nil {
						content = data
					}
					reader.Close()
				}
			}
		}
	}

	err := n.ofs.CreateFile(childPath, content, os.FileMode(mode))
	if err != nil {
		log(ctx).Errorf("error creating file %v: %v", name, err)
		return nil, nil, 0, syscall.EIO
	}

	// Get the newly created entry
	entry, err := n.ofs.GetEntry(ctx, childPath)
	if err != nil {
		log(ctx).Errorf("error getting created file %v: %v", name, err)
		return nil, nil, 0, syscall.EIO
	}

	fileEntry, ok := entry.(fs.File)
	if !ok {
		return nil, nil, 0, syscall.EIO
	}

	stable := gofusefs.StableAttr{
		Mode: fuse.S_IFREG,
	}

	node := NewWritableFileNode(fileEntry, n.ofs, childPath)
	child := n.NewInode(ctx, node, stable)

	populateAttributesFromEntry(&out.Attr, entry)

	// Create file handle for writing
	fh := &WritableFileHandle{
		ofs:   n.ofs,
		path:  childPath,
		flags: flags,
	}

	return child, fh, 0, gofusefs.OK
}

// Unlink implements the NodeUnlinker interface.
func (n *WritableDirectoryNode) Unlink(ctx context.Context, name string) syscall.Errno {
	childPath := n.path
	if childPath == "" {
		childPath = name
	} else {
		childPath = childPath + "/" + name
	}

	err := n.ofs.DeleteFile(childPath)
	if err != nil {
		log(ctx).Errorf("error unlinking file %v: %v", name, err)
		return syscall.EIO
	}

	return gofusefs.OK
}

// Mkdir implements the NodeMkdirer interface.
func (n *WritableDirectoryNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*gofusefs.Inode, syscall.Errno) {
	childPath := n.path
	if childPath == "" {
		childPath = name
	} else {
		childPath = childPath + "/" + name
	}

	err := n.ofs.CreateDirectory(childPath, os.FileMode(mode))
	if err != nil {
		log(ctx).Errorf("error creating directory %v: %v", name, err)
		return nil, syscall.EIO
	}

	// Get the newly created directory entry
	entry, err := n.ofs.GetEntry(ctx, childPath)
	if err != nil {
		log(ctx).Errorf("error getting created directory %v: %v", name, err)
		return nil, syscall.EIO
	}

	dirEntry, ok := entry.(fs.Directory)
	if !ok {
		return nil, syscall.EIO
	}

	stable := gofusefs.StableAttr{
		Mode: fuse.S_IFDIR,
	}

	node := NewWritableDirectoryNode(dirEntry, n.ofs, childPath)
	child := n.NewInode(ctx, node, stable)

	populateAttributesFromEntry(&out.Attr, entry)

	return child, gofusefs.OK
}

// Rmdir implements the NodeRmdirer interface.
func (n *WritableDirectoryNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	childPath := n.path
	if childPath == "" {
		childPath = name
	} else {
		childPath = childPath + "/" + name
	}

	err := n.ofs.DeleteFile(childPath) // DeleteFile works for directories too
	if err != nil {
		log(ctx).Errorf("error removing directory %v: %v", name, err)
		return syscall.EIO
	}

	return gofusefs.OK
}

// Open implements the NodeOpener interface for files.
func (n *WritableFileNode) Open(ctx context.Context, flags uint32) (gofusefs.FileHandle, uint32, syscall.Errno) {
	// Validate flags against current file state
	existsInOverlay := false
	existsInSnapshot := false
	isDeleted := n.ofs.isFileDeleted(n.path)

	// Check overlay layer
	if _, err := n.ofs.getOverlayFile(n.path); err == nil {
		existsInOverlay = true
	}

	// Check snapshot layer (only if not deleted and not in overlay)
	if !existsInOverlay && !isDeleted {
		if _, err := n.ofs.findEntryInSnapshot(ctx, n.path); err == nil {
			existsInSnapshot = true
		}
	}

	// Determine if file is visible (exists and not deleted)
	fileVisible := existsInOverlay || (existsInSnapshot && !isDeleted)

	// Handle O_CREAT and O_EXCL flag validation
	if flags&syscall.O_CREAT != 0 {
		if flags&syscall.O_EXCL != 0 && fileVisible {
			// O_CREAT|O_EXCL specified but file exists
			log(ctx).Debugf("Open failed: file exists (O_CREAT|O_EXCL) %v", n.path)
			return nil, 0, syscall.EEXIST
		}
		// O_CREAT without O_EXCL is fine whether file exists or not
	} else {
		// No O_CREAT specified - file must exist
		if !fileVisible {
			log(ctx).Debugf("Open failed: file does not exist (no O_CREAT) %v", n.path)
			return nil, 0, syscall.ENOENT
		}
	}

	fh := &WritableFileHandle{
		ofs:   n.ofs,
		path:  n.path,
		flags: flags,
	}

	return fh, 0, gofusefs.OK
}

// Read implements the FileReader interface.
func (fh *WritableFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	// If we have an active write file handle, use it for reading to avoid consistency issues
	if fh.file != nil {
		// Ensure file is synced before reading
		if err := fh.file.Sync(); err != nil {
			log(ctx).Errorf("sync error before read %v: %v", fh.path, err)
			return nil, syscall.EIO
		}

		n, err := fh.file.ReadAt(dest, off)
		if err != nil && !errors.Is(err, io.EOF) {
			log(ctx).Errorf("read error from write handle %v: %v", fh.path, err)
			return nil, syscall.EIO
		}

		return fuse.ReadResultData(dest[:n]), gofusefs.OK
	}

	// No active write handle, use standard overlay read path
	entry, err := fh.ofs.GetEntry(ctx, fh.path)
	if err != nil {
		log(ctx).Errorf("read error getting entry %v: %v", fh.path, err)
		return nil, syscall.EIO
	}

	fileEntry, ok := entry.(fs.File)
	if !ok {
		return nil, syscall.EIO
	}

	reader, err := fileEntry.Open(ctx)
	if err != nil {
		log(ctx).Errorf("read error opening %v: %v", fh.path, err)
		return nil, syscall.EIO
	}
	defer func() { _ = reader.Close() }()

	_, err = reader.Seek(off, 0)
	if err != nil {
		log(ctx).Errorf("seek error: %v %v: %v", fh.path, off, err)
		return nil, syscall.EIO
	}

	n, err := reader.Read(dest)
	if err != nil && !errors.Is(err, io.EOF) {
		log(ctx).Errorf("read error: %v: %v", fh.path, err)
		return nil, syscall.EIO
	}

	return fuse.ReadResultData(dest[0:n]), gofusefs.OK
}

// Write implements the FileWriter interface.
func (fh *WritableFileHandle) Write(ctx context.Context, data []byte, off int64) (written uint32, errno syscall.Errno) {
	if err := fh.ensureWritableFile(ctx); err != nil {
		log(ctx).Errorf("write error ensuring writable file %v: %v", fh.path, err)
		return 0, syscall.EIO
	}

	n, err := fh.file.WriteAt(data, off)
	if err != nil {
		log(ctx).Errorf("write error: %v: %v", fh.path, err)
		return 0, syscall.EIO
	}

	fh.modified = true
	return uint32(n), gofusefs.OK //nolint:gosec
}

// Flush implements the FileFlusher interface.
func (fh *WritableFileHandle) Flush(_ context.Context) syscall.Errno {
	if fh.file != nil {
		// Sync the file to ensure all writes are flushed
		if err := fh.file.Sync(); err != nil {
			return syscall.EIO
		}

		// Close the file handle to release the lock and make it available for other readers
		// If needed again, ensureWritableFile will reopen it
		fh.file.Close() //nolint:errcheck // Flush should not fail
		fh.file = nil
	}
	return gofusefs.OK
}

// Release implements the FileReleaser interface.
func (fh *WritableFileHandle) Release(_ context.Context) syscall.Errno {
	if fh.file != nil {
		fh.file.Close() //nolint:errcheck // Release should not fail
		fh.file = nil
	}
	return gofusefs.OK
}

// ensureWritableFile ensures we have an open writable file handle in overlay.
func (fh *WritableFileHandle) ensureWritableFile(ctx context.Context) error {
	if fh.file != nil {
		return nil // Already open
	}

	// Get overlay path for this file
	overlayPath := fh.ofs.getOverlayFilePath(fh.path)

	// Check current file state
	overlayExists := false
	if _, err := os.Stat(overlayPath); err == nil {
		overlayExists = true
	}

	// Handle file creation/copying logic based on overlay state
	if !overlayExists {
		// File doesn't exist in overlay yet
		if fh.flags&syscall.O_CREAT != 0 {
			// O_CREAT specified - copy from snapshot if it exists, otherwise create empty
			if err := fh.copyToOverlay(ctx); err != nil {
				return err
			}
		} else {
			// No O_CREAT - file must exist somewhere accessible, this is an error state
			return errors.New("file does not exist in overlay and O_CREAT not specified")
		}
	}

	// Determine proper flags for opening the overlay file
	openFlags := int(fh.flags)

	// Since we now guarantee the file exists in overlay, remove O_CREAT and O_EXCL
	openFlags &^= (syscall.O_CREAT | syscall.O_EXCL)

	// Ensure we have write capability if this is a write operation
	if openFlags&syscall.O_WRONLY == 0 && openFlags&syscall.O_RDWR == 0 {
		// Add O_RDWR for writing capability
		openFlags |= syscall.O_RDWR
	}

	file, err := os.OpenFile(overlayPath, openFlags, 0o644) //nolint:mnd,gosec
	if err != nil {
		return errors.Wrap(err, "failed to open overlay file for writing")
	}

	fh.file = file
	return nil
}

// copyToOverlay copies the current file content to overlay if it doesn't exist there.
func (fh *WritableFileHandle) copyToOverlay(ctx context.Context) error {
	// Get current entry from snapshot
	entry, err := fh.ofs.findEntryInSnapshot(ctx, fh.path)
	if err != nil {
		// File might not exist in snapshot, create empty file
		return fh.ofs.CreateFile(fh.path, []byte{}, 0o644) //nolint:mnd
	}

	fileEntry, ok := entry.(fs.File)
	if !ok {
		return errors.New("not a file")
	}

	// Read content from snapshot
	reader, err := fileEntry.Open(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to open snapshot file")
	}
	defer func() { _ = reader.Close() }()

	content := make([]byte, entry.Size())
	_, err = reader.Read(content)
	if err != nil {
		return errors.Wrap(err, "failed to read snapshot file")
	}

	// Write to overlay
	return fh.ofs.CreateFile(fh.path, content, entry.Mode())
}

// Setattr implements the NodeSetattrer interface for directories.
func (n *WritableDirectoryNode) Setattr(ctx context.Context, _ gofusefs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return n.setAttributes(ctx, in, out)
}

// Setattr implements the NodeSetattrer interface for files.
func (n *WritableFileNode) Setattr(ctx context.Context, _ gofusefs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return n.setAttributes(ctx, in, out)
}

// setAttributes handles attribute setting for files.
//
//nolint:gocyclo // This function handles multiple attribute types but is still readable
func (n *WritableFileNode) setAttributes(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	overlayPath := n.ofs.getOverlayFilePath(n.path)

	// Ensure file exists in overlay
	if _, err := os.Stat(overlayPath); os.IsNotExist(err) {
		if err := n.copyToOverlay(ctx); err != nil {
			log(ctx).Errorf("setattr error copying to overlay %v: %v", n.path, err)
			return syscall.EIO
		}
	}

	// Handle size changes (truncate)
	if in.Valid&fuse.FATTR_SIZE != 0 {
		if err := os.Truncate(overlayPath, int64(in.Size)); err != nil { //nolint:gosec
			log(ctx).Errorf("setattr truncate error %v: %v", n.path, err)
			return syscall.EIO
		}
	}

	// Handle mode changes (chmod)
	if in.Valid&fuse.FATTR_MODE != 0 {
		mode := os.FileMode(in.Mode & 0o7777) //nolint:mnd // Standard permission mask
		if err := os.Chmod(overlayPath, mode); err != nil {
			log(ctx).Errorf("setattr chmod error %v: %v", n.path, err)
			return syscall.EIO
		}
	}

	// Handle ownership changes (chown)
	if (in.Valid&fuse.FATTR_UID != 0) || (in.Valid&fuse.FATTR_GID != 0) {
		uid := -1
		gid := -1
		if in.Valid&fuse.FATTR_UID != 0 {
			uid = int(in.Uid)
		}
		if in.Valid&fuse.FATTR_GID != 0 {
			gid = int(in.Gid)
		}
		if err := os.Chown(overlayPath, uid, gid); err != nil {
			log(ctx).Errorf("setattr chown error %v: %v", n.path, err)
			return syscall.EIO
		}
	}

	// Handle time changes (utimes)
	if (in.Valid&fuse.FATTR_ATIME != 0) || (in.Valid&fuse.FATTR_MTIME != 0) {
		atime := time.Unix(int64(in.Atime), int64(in.Atimensec)) //nolint:gosec
		mtime := time.Unix(int64(in.Mtime), int64(in.Mtimensec)) //nolint:gosec
		if err := os.Chtimes(overlayPath, atime, mtime); err != nil {
			log(ctx).Errorf("setattr chtimes error %v: %v", n.path, err)
			return syscall.EIO
		}
	}

	// Get updated entry and populate attributes
	entry, err := n.ofs.GetEntry(ctx, n.path)
	if err != nil {
		log(ctx).Errorf("setattr error getting updated entry %v: %v", n.path, err)
		return syscall.EIO
	}

	populateAttributesFromEntry(&out.Attr, entry)
	return gofusefs.OK
}

// setAttributes handles attribute setting for directories.
func (n *WritableDirectoryNode) setAttributes(_ context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	// For directories, we need to create them in overlay if they don't exist
	// This is more complex as we'd need to recreate the directory structure
	// For now, return success without changes for compatibility
	populateAttributesFromEntry(&out.Attr, n.entry)
	return gofusefs.OK
}

// copyToOverlay copies the current file content to overlay if it doesn't exist there.
func (n *WritableFileNode) copyToOverlay(ctx context.Context) error {
	// Similar to WritableFileHandle.copyToOverlay but for the file node
	entry, err := n.ofs.findEntryInSnapshot(ctx, n.path)
	if err != nil {
		return n.ofs.CreateFile(n.path, []byte{}, 0o644) //nolint:mnd
	}

	fileEntry, ok := entry.(fs.File)
	if !ok {
		return errors.New("not a file")
	}

	reader, err := fileEntry.Open(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to open snapshot file")
	}
	defer func() { _ = reader.Close() }()

	content := make([]byte, entry.Size())
	_, err = reader.Read(content)
	if err != nil {
		return errors.Wrap(err, "failed to read snapshot file")
	}

	return n.ofs.CreateFile(n.path, content, entry.Mode())
}

// Getxattr implements NodeGetxattrer interface for extended attributes.
func (n *WritableDirectoryNode) Getxattr(_ context.Context, _ string, _ []byte) (uint32, syscall.Errno) {
	// Extended attributes are not supported in the overlay filesystem
	return 0, syscall.ENOTSUP
}

// Setxattr implements NodeSetxattrer interface for extended attributes.
func (n *WritableDirectoryNode) Setxattr(_ context.Context, _ string, _ []byte, _ uint32) syscall.Errno {
	// Extended attributes are not supported in the overlay filesystem
	return syscall.ENOTSUP
}

// Removexattr implements NodeRemovexattrer interface for extended attributes.
func (n *WritableDirectoryNode) Removexattr(_ context.Context, _ string) syscall.Errno {
	// Extended attributes are not supported in the overlay filesystem
	return syscall.ENOTSUP
}

// Listxattr implements NodeListxattrer interface for extended attributes.
func (n *WritableDirectoryNode) Listxattr(_ context.Context, _ []byte) (uint32, syscall.Errno) {
	// Extended attributes are not supported in the overlay filesystem
	return 0, syscall.ENOTSUP
}

// Getxattr implements NodeGetxattrer interface for extended attributes.
func (n *WritableFileNode) Getxattr(_ context.Context, _ string, _ []byte) (uint32, syscall.Errno) {
	// Extended attributes are not supported in the overlay filesystem
	return 0, syscall.ENOTSUP
}

// Setxattr implements NodeSetxattrer interface for extended attributes.
func (n *WritableFileNode) Setxattr(_ context.Context, _ string, _ []byte, _ uint32) syscall.Errno {
	// Extended attributes are not supported in the overlay filesystem
	return syscall.ENOTSUP
}

// Removexattr implements NodeRemovexattrer interface for extended attributes.
func (n *WritableFileNode) Removexattr(_ context.Context, _ string) syscall.Errno {
	// Extended attributes are not supported in the overlay filesystem
	return syscall.ENOTSUP
}

// Listxattr implements NodeListxattrer interface for extended attributes.
func (n *WritableFileNode) Listxattr(_ context.Context, _ []byte) (uint32, syscall.Errno) {
	// Extended attributes are not supported in the overlay filesystem
	return 0, syscall.ENOTSUP
}

// Access implements NodeAccesser interface for permission checking.
func (n *WritableFileNode) Access(_ context.Context, mask uint32) syscall.Errno {
	// In writable mode, allow all access for owner
	return gofusefs.OK
}

// Verify interface compliance at compile time.
var (
	_ gofusefs.InodeEmbedder     = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeGetattrer     = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeLookuper      = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeReaddirer     = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeAccesser      = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeCreater       = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeUnlinker      = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeMkdirer       = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeRmdirer       = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeSetattrer     = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeGetxattrer    = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeSetxattrer    = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeRemovexattrer = (*WritableDirectoryNode)(nil)
	_ gofusefs.NodeListxattrer   = (*WritableDirectoryNode)(nil)
	_ gofusefs.InodeEmbedder     = (*WritableFileNode)(nil)
	_ gofusefs.NodeGetattrer     = (*WritableFileNode)(nil)
	_ gofusefs.NodeOpener        = (*WritableFileNode)(nil)
	_ gofusefs.NodeSetattrer     = (*WritableFileNode)(nil)
	_ gofusefs.NodeAccesser      = (*WritableFileNode)(nil)
	_ gofusefs.NodeGetxattrer    = (*WritableFileNode)(nil)
	_ gofusefs.NodeSetxattrer    = (*WritableFileNode)(nil)
	_ gofusefs.NodeRemovexattrer = (*WritableFileNode)(nil)
	_ gofusefs.NodeListxattrer   = (*WritableFileNode)(nil)
	_ gofusefs.FileHandle        = (*WritableFileHandle)(nil)
	_ gofusefs.FileReader        = (*WritableFileHandle)(nil)
	_ gofusefs.FileWriter        = (*WritableFileHandle)(nil)
	_ gofusefs.FileFlusher       = (*WritableFileHandle)(nil)
	_ gofusefs.FileReleaser      = (*WritableFileHandle)(nil)
)
