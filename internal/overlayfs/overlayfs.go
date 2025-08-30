// Package overlayfs implements a read-through overlay filesystem for Kopia mounts.
// It provides a layered view where deleted files, overlay files, and snapshot files
// are checked in order to determine the current state.
package overlayfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo/logging"
)

var (
	log = logging.Module("overlayfs")

	// Static errors.
	errNotDirectory  = errors.New("path component is not a directory")
	errPathTraversal = errors.New("unexpected end of path traversal")
)

// OverlayFS provides a read-through overlay filesystem that combines
// deleted files, overlay files, and snapshot files in a layered approach.
type OverlayFS struct {
	baseDir    fs.Directory
	mountPoint string
	overlayDir string
	writtenDir string
	deletedDir string
}

// New creates a new OverlayFS instance.
func New(baseDir fs.Directory, mountPoint string) (*OverlayFS, error) {
	// Create overlay directory parallel to mount point to avoid conflicts
	mountDir := filepath.Dir(mountPoint)
	mountName := filepath.Base(mountPoint)
	overlayDir := filepath.Join(mountDir, "."+mountName+".overlay")
	writtenDir := filepath.Join(overlayDir, "written")
	deletedDir := filepath.Join(overlayDir, "deleted")

	ofs := &OverlayFS{
		baseDir:    baseDir,
		mountPoint: mountPoint,
		overlayDir: overlayDir,
		writtenDir: writtenDir,
		deletedDir: deletedDir,
	}

	// Ensure overlay directories exist
	if err := ofs.ensureOverlayDirs(); err != nil {
		return nil, errors.Wrap(err, "failed to create overlay directories")
	}

	return ofs, nil
}

// ensureOverlayDirs creates the necessary overlay directory structure.
func (ofs *OverlayFS) ensureOverlayDirs() error {
	dirs := []string{ofs.overlayDir, ofs.writtenDir, ofs.deletedDir}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:mnd // Standard directory permission //nolint:mnd // Standard directory permission
			return errors.Wrapf(err, "failed to create directory: %s", dir)
		}
	}

	return nil
}

// isFileDeleted checks if a file is marked as deleted in the overlay.
func (ofs *OverlayFS) isFileDeleted(relativePath string) bool {
	deletedPath := filepath.Join(ofs.deletedDir, relativePath)
	_, err := os.Stat(deletedPath)

	return err == nil
}

// getOverlayFile gets a file from the overlay written directory if it exists.
func (ofs *OverlayFS) getOverlayFile(relativePath string) (fs.Entry, error) {
	overlayPath := filepath.Join(ofs.writtenDir, relativePath)

	// Check if file exists in overlay and get file info
	fileInfo, err := os.Stat(overlayPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, errors.Wrap(err, "failed to stat overlay file")
	}

	// Check if it's a directory and handle accordingly
	if fileInfo.IsDir() {
		return &overlayDirectoryEntry{
			name:         filepath.Base(relativePath),
			overlayPath:  overlayPath,
			fileInfo:     fileInfo,
			ofs:          ofs,
			relativePath: relativePath,
		}, nil
	}

	return &overlayFileEntry{
		name:        filepath.Base(relativePath),
		overlayPath: overlayPath,
		fileInfo:    fileInfo,
	}, nil
}

// GetEntry implements the layered lookup: deleted -> overlay -> snapshot.
func (ofs *OverlayFS) GetEntry(ctx context.Context, relativePath string) (fs.Entry, error) {
	// 1. Check if file is deleted
	if ofs.isFileDeleted(relativePath) {
		return nil, os.ErrNotExist
	}

	// 2. Check if file exists in overlay
	if entry, err := ofs.getOverlayFile(relativePath); err == nil {
		log(ctx).Debugf("Found file in overlay: %s", relativePath)
		return entry, nil
	}

	// 3. Fall back to snapshot
	log(ctx).Debugf("File not in overlay, checking snapshot: %s", relativePath)

	entry, err := ofs.findEntryInSnapshot(ctx, relativePath)
	if err != nil {
		// Ensure we always return os.ErrNotExist for not found files
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		// For other errors, still return os.ErrNotExist to avoid EIO in FUSE
		log(ctx).Debugf("Treating error as not found for %s: %v", relativePath, err)
		return nil, os.ErrNotExist
	}

	return entry, nil
}

// findEntryInSnapshot finds a file entry in the base snapshot by relative path.
func (ofs *OverlayFS) findEntryInSnapshot(ctx context.Context, relativePath string) (fs.Entry, error) {
	// Handle root path
	if relativePath == "" || relativePath == "." || relativePath == "/" {
		return ofs.baseDir, nil
	}

	// Split the path into components using filepath.Split and strings.Split
	// Clean the path first to handle .. and . properly
	cleanPath := filepath.Clean(relativePath)
	// Remove leading slash if present
	if cleanPath != "" && cleanPath[0] == '/' {
		cleanPath = cleanPath[1:]
	}

	components := []string{}
	if cleanPath != "" {
		components = strings.Split(cleanPath, "/")
	}

	if len(components) == 0 {
		return ofs.baseDir, nil
	}

	// Navigate through the directory structure
	current := ofs.baseDir

	for i, component := range components {
		if i == len(components)-1 {
			// Last component, this should be the file
			entry, err := current.Child(ctx, component)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, os.ErrNotExist
				}
				return nil, errors.Wrapf(err, "failed to find file: %s", component)
			}

			return entry, nil
		}

		// Intermediate component, should be a directory
		entry, err := current.Child(ctx, component)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, os.ErrNotExist
			}
			return nil, errors.Wrapf(err, "failed to find component: %s", component)
		}

		dir, ok := entry.(fs.Directory)
		if !ok {
			return nil, errors.Wrapf(errNotDirectory, "component: %s", component)
		}

		current = dir
	}

	return nil, errPathTraversal
}

// GetOverlayPath returns the path to the overlay directory.
func (ofs *OverlayFS) GetOverlayPath() string {
	return ofs.overlayDir
}

// GetWrittenPath returns the path to the written directory.
func (ofs *OverlayFS) GetWrittenPath() string {
	return ofs.writtenDir
}

// GetDeletedPath returns the path to the deleted directory.
func (ofs *OverlayFS) GetDeletedPath() string {
	return ofs.deletedDir
}

// overlayFileEntry represents a file that exists in the overlay.
type overlayFileEntry struct {
	name        string
	overlayPath string
	fileInfo    os.FileInfo
}

func (f *overlayFileEntry) Name() string { return f.name }
func (f *overlayFileEntry) Size() int64 {
	// Get fresh file info to ensure accurate size reporting
	if info, err := os.Stat(f.overlayPath); err == nil {
		return info.Size()
	}

	// Fallback to cached fileInfo if stat fails
	if f.fileInfo != nil {
		return f.fileInfo.Size()
	}

	return 0
}

func (f *overlayFileEntry) Mode() os.FileMode {
	if f.fileInfo != nil {
		return f.fileInfo.Mode()
	}

	return 0o644 //nolint:mnd // Standard file permission
}

func (f *overlayFileEntry) ModTime() time.Time {
	// Get fresh file info to ensure accurate modification time
	if info, err := os.Stat(f.overlayPath); err == nil {
		return info.ModTime()
	}

	// Fallback to cached fileInfo if stat fails
	if f.fileInfo != nil {
		return f.fileInfo.ModTime()
	}

	return time.Time{}
}

func (f *overlayFileEntry) IsDir() bool {
	if f.fileInfo != nil {
		return f.fileInfo.IsDir()
	}

	return false
}
func (f *overlayFileEntry) Owner() fs.OwnerInfo         { return fs.OwnerInfo{} }
func (f *overlayFileEntry) Device() fs.DeviceInfo       { return fs.DeviceInfo{} }
func (f *overlayFileEntry) LocalFilesystemPath() string { return f.overlayPath }
func (f *overlayFileEntry) Close()                      {}
func (f *overlayFileEntry) Open(_ context.Context) (fs.Reader, error) {
	file, err := os.Open(f.overlayPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open overlay file")
	}

	return &overlayFileReader{file: file}, nil
}

func (f *overlayFileEntry) Sys() interface{} {
	return f.fileInfo
}

// overlayFileReader implements fs.Reader for overlay files.
type overlayFileReader struct {
	file *os.File
}

func (r *overlayFileReader) Read(p []byte) (n int, err error) {
	return r.file.Read(p) //nolint:wrapcheck // EOF is expected behavior
}

func (r *overlayFileReader) Close() error {
	if err := r.file.Close(); err != nil {
		return errors.Wrap(err, "failed to close overlay file")
	}

	return nil
}

func (r *overlayFileReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := r.file.Seek(offset, whence)
	if err != nil {
		return pos, errors.Wrap(err, "failed to seek in overlay file")
	}

	return pos, nil
}

func (r *overlayFileReader) Entry() (fs.Entry, error) {
	return nil, nil
}

// overlayDirectoryEntry represents a directory that exists in the overlay.
type overlayDirectoryEntry struct {
	name         string
	overlayPath  string
	fileInfo     os.FileInfo
	ofs          *OverlayFS
	relativePath string
}

func (d *overlayDirectoryEntry) Name() string { return d.name }
func (d *overlayDirectoryEntry) Size() int64 {
	if d.fileInfo != nil {
		return d.fileInfo.Size()
	}

	return 0
}

func (d *overlayDirectoryEntry) Mode() os.FileMode {
	if d.fileInfo != nil {
		return d.fileInfo.Mode()
	}

	return os.ModeDir | 0o755 //nolint:mnd // Standard directory permission
}

func (d *overlayDirectoryEntry) ModTime() time.Time {
	if d.fileInfo != nil {
		return d.fileInfo.ModTime()
	}

	return time.Time{}
}
func (d *overlayDirectoryEntry) IsDir() bool                 { return true }
func (d *overlayDirectoryEntry) Owner() fs.OwnerInfo         { return fs.OwnerInfo{} }
func (d *overlayDirectoryEntry) Device() fs.DeviceInfo       { return fs.DeviceInfo{} }
func (d *overlayDirectoryEntry) LocalFilesystemPath() string { return d.overlayPath }
func (d *overlayDirectoryEntry) Close()                      {}
func (d *overlayDirectoryEntry) Open(_ context.Context) (fs.Reader, error) {
	return nil, errors.New("cannot open directory for reading")
}

func (d *overlayDirectoryEntry) Sys() interface{} {
	return d.fileInfo
}

// Child implements fs.Directory interface for overlay directories.
func (d *overlayDirectoryEntry) Child(ctx context.Context, name string) (fs.Entry, error) {
	childPath := filepath.Join(d.relativePath, name)
	if d.relativePath == "" {
		childPath = name
	}

	return d.ofs.GetEntry(ctx, childPath)
}

// Iterate implements fs.Directory interface for overlay directories.
func (d *overlayDirectoryEntry) Iterate(_ context.Context) (fs.DirectoryIterator, error) {
	return &overlayDirectoryIterator{
		ofs:          d.ofs,
		relativePath: d.relativePath,
	}, nil
}

// IterateEntries implements fs.Directory interface for overlay directories.
func (d *overlayDirectoryEntry) SupportsMultipleIterations() bool { return true }

func (d *overlayDirectoryEntry) IterateEntries(ctx context.Context, callback func(fs.Entry) error) error {
	iter, err := d.Iterate(ctx)
	if err != nil {
		return err
	}
	defer iter.Close()

	for {
		entry, err := iter.Next(ctx)
		if entry == nil {
			break
		}

		if err != nil {
			return err //nolint:wrapcheck // Iterator error passthrough
		}

		if err := callback(entry); err != nil {
			return err
		}
	}

	return nil
}

// overlayDirectoryIterator implements directory iteration with overlay support.
type overlayDirectoryIterator struct {
	ofs          *OverlayFS
	relativePath string
	entries      []fs.Entry
	index        int
	initialized  bool
}

func (it *overlayDirectoryIterator) Next(ctx context.Context) (fs.Entry, error) {
	if !it.initialized {
		it.buildEntryList(ctx)
		it.initialized = true
	}

	if it.index >= len(it.entries) {
		return nil, nil
	}

	entry := it.entries[it.index]
	it.index++

	return entry, nil
}

func (it *overlayDirectoryIterator) Close() {
	it.entries = nil
}

func (it *overlayDirectoryIterator) buildEntryList(ctx context.Context) {
	entryMap := make(map[string]fs.Entry)

	// 1. Start with snapshot entries (if base directory exists)
	if baseEntry, err := it.ofs.findEntryInSnapshot(ctx, it.relativePath); err == nil {
		if baseDir, ok := baseEntry.(fs.Directory); ok {
			err := fs.IterateEntries(ctx, baseDir, func(_ context.Context, entry fs.Entry) error {
				// Wrap directories to ensure they can be navigated through overlay system
				if dir, isDir := entry.(fs.Directory); isDir {
					childRelPath := entry.Name()
					if it.relativePath != "" {
						childRelPath = filepath.Join(it.relativePath, entry.Name())
					}

					wrappedDir := &snapshotDirectoryWrapper{
						dir:          dir,
						ofs:          it.ofs,
						relativePath: childRelPath,
					}
					entryMap[entry.Name()] = wrappedDir
				} else {
					// Files don't need wrapping
					entryMap[entry.Name()] = entry
				}
				return nil
			})
			if err != nil {
				log(ctx).Debugf("Error iterating base directory: %v", err)
			}
		}
	}

	// 2. Add/override with overlay entries
	overlayDirPath := filepath.Join(it.ofs.writtenDir, it.relativePath)
	if entries, err := os.ReadDir(overlayDirPath); err == nil {
		for _, entry := range entries {
			childRelPath := entry.Name()
			if it.relativePath != "" {
				childRelPath = filepath.Join(it.relativePath, entry.Name())
			}

			if overlayEntry, err := it.ofs.getOverlayFile(childRelPath); err == nil {
				entryMap[entry.Name()] = overlayEntry
			}
		}
	}

	// 3. Remove deleted entries
	deletedDirPath := filepath.Join(it.ofs.deletedDir, it.relativePath)
	if entries, err := os.ReadDir(deletedDirPath); err == nil {
		for _, entry := range entries {
			delete(entryMap, entry.Name())
		}
	}

	// Convert map to slice
	it.entries = make([]fs.Entry, 0, len(entryMap))
	for _, entry := range entryMap {
		it.entries = append(it.entries, entry)
	}
}

// GetRootDirectory returns the root directory with overlay support.
func (ofs *OverlayFS) GetRootDirectory(_ context.Context) (fs.Directory, error) {
	// Always return the base directory wrapped with overlay support
	// This ensures we get both snapshot and overlay content
	return &snapshotDirectoryWrapper{
		dir:          ofs.baseDir,
		ofs:          ofs,
		relativePath: "",
	}, nil
}

// snapshotDirectoryWrapper wraps a snapshot directory to provide overlay support.
type snapshotDirectoryWrapper struct {
	dir          fs.Directory
	ofs          *OverlayFS
	relativePath string
}

func (w *snapshotDirectoryWrapper) Name() string                { return w.dir.Name() }
func (w *snapshotDirectoryWrapper) Size() int64                 { return w.dir.Size() }
func (w *snapshotDirectoryWrapper) Mode() os.FileMode           { return w.dir.Mode() }
func (w *snapshotDirectoryWrapper) ModTime() time.Time          { return w.dir.ModTime() }
func (w *snapshotDirectoryWrapper) IsDir() bool                 { return true }
func (w *snapshotDirectoryWrapper) Owner() fs.OwnerInfo         { return w.dir.Owner() }
func (w *snapshotDirectoryWrapper) Device() fs.DeviceInfo       { return w.dir.Device() }
func (w *snapshotDirectoryWrapper) LocalFilesystemPath() string { return w.dir.LocalFilesystemPath() }
func (w *snapshotDirectoryWrapper) Close()                      { w.dir.Close() }
func (w *snapshotDirectoryWrapper) Open(_ context.Context) (fs.Reader, error) {
	return nil, errors.New("cannot open directory for reading")
}
func (w *snapshotDirectoryWrapper) Sys() interface{} { return w.dir.Sys() }

func (w *snapshotDirectoryWrapper) Child(ctx context.Context, name string) (fs.Entry, error) {
	childPath := filepath.Join(w.relativePath, name)
	if w.relativePath == "" {
		childPath = name
	}

	return w.ofs.GetEntry(ctx, childPath)
}

func (w *snapshotDirectoryWrapper) Iterate(_ context.Context) (fs.DirectoryIterator, error) {
	return &overlayDirectoryIterator{
		ofs:          w.ofs,
		relativePath: w.relativePath,
	}, nil
}

func (w *snapshotDirectoryWrapper) SupportsMultipleIterations() bool { return true }

func (w *snapshotDirectoryWrapper) IterateEntries(ctx context.Context, callback func(fs.Entry) error) error {
	iter, err := w.Iterate(ctx)
	if err != nil {
		return err
	}
	defer iter.Close()

	for {
		entry, err := iter.Next(ctx)
		if entry == nil {
			break
		}

		if err != nil {
			return err //nolint:wrapcheck // Iterator error passthrough
		}

		if err := callback(entry); err != nil {
			return err
		}
	}

	return nil
}

// CreateFile creates a new file in the overlay.
func (ofs *OverlayFS) CreateFile(relativePath string, content []byte, mode os.FileMode) error {
	overlayPath := filepath.Join(ofs.writtenDir, relativePath)

	// Ensure parent directories exist
	if err := os.MkdirAll(filepath.Dir(overlayPath), 0o750); err != nil { //nolint:mnd // Standard directory permission
		return errors.Wrap(err, "failed to create parent directories")
	}

	// Write the file
	if err := os.WriteFile(overlayPath, content, mode); err != nil {
		return errors.Wrap(err, "failed to write overlay file")
	}

	// Remove from deleted list if it was there
	deletedPath := filepath.Join(ofs.deletedDir, relativePath)
	_ = os.Remove(deletedPath) // Ignore error, file may not exist

	return nil
}

// DeleteFile marks a file as deleted in the overlay.
func (ofs *OverlayFS) DeleteFile(relativePath string) error {
	deletedPath := filepath.Join(ofs.deletedDir, relativePath)

	// Ensure parent directories exist
	if err := os.MkdirAll(filepath.Dir(deletedPath), 0o750); err != nil { //nolint:mnd // Standard directory permission
		return errors.Wrap(err, "failed to create parent directories")
	}

	// Create empty marker file
	if err := os.WriteFile(deletedPath, []byte{}, 0o600); err != nil { //nolint:mnd // Standard file permission
		return errors.Wrap(err, "failed to create delete marker")
	}

	// Remove from overlay if it was there
	overlayPath := filepath.Join(ofs.writtenDir, relativePath)
	_ = os.Remove(overlayPath) // Ignore error, file may not exist

	return nil
}

// CreateDirectory creates a directory in the overlay filesystem.
func (ofs *OverlayFS) CreateDirectory(path string, mode os.FileMode) error {
	overlayPath := ofs.getOverlayFilePath(path)

	if err := os.MkdirAll(filepath.Dir(overlayPath), 0o750); err != nil {
		return errors.Wrap(err, "failed to create parent directories")
	}

	if err := os.Mkdir(overlayPath, mode); err != nil {
		return errors.Wrap(err, "failed to create directory")
	}

	return nil
}

// getOverlayFilePath returns the path to a file in the overlay filesystem.
func (ofs *OverlayFS) getOverlayFilePath(path string) string {
	return filepath.Join(ofs.writtenDir, path)
}
