package overlayfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/internal/mockfs"
)

func TestNewCreatesOverlayParallelToMountPoint(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "overlayfs-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a mock directory as the base
	mockDir := mockfs.NewDirectory()

	// Create mount point inside temp dir
	mountPoint := filepath.Join(tempDir, "test-mount")
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		t.Fatalf("Failed to create mount point: %v", err)
	}

	// Create overlay filesystem
	overlayFS, err := New(mockDir, mountPoint)
	if err != nil {
		t.Fatalf("Failed to create overlay filesystem: %v", err)
	}

	// Verify overlay directory is parallel to mount point, not inside it
	expectedOverlayDir := filepath.Join(tempDir, ".test-mount.overlay")
	if overlayFS.GetOverlayPath() != expectedOverlayDir {
		t.Errorf("Expected overlay directory to be %s, got %s", expectedOverlayDir, overlayFS.GetOverlayPath())
	}

	// Verify overlay directory exists
	if _, err := os.Stat(expectedOverlayDir); err != nil {
		t.Errorf("Expected overlay directory to exist: %v", err)
	}

	// Verify overlay directory is NOT inside mount point
	overlayInsideMount := filepath.Join(mountPoint, ".overlay")
	if _, err := os.Stat(overlayInsideMount); err == nil {
		t.Errorf("Overlay directory should not exist inside mount point at %s", overlayInsideMount)
	}

	// Verify written and deleted subdirectories exist
	writtenDir := filepath.Join(expectedOverlayDir, "written")
	if _, err := os.Stat(writtenDir); err != nil {
		t.Errorf("Expected written directory to exist: %v", err)
	}

	deletedDir := filepath.Join(expectedOverlayDir, "deleted")
	if _, err := os.Stat(deletedDir); err != nil {
		t.Errorf("Expected deleted directory to exist: %v", err)
	}
}

func TestNewWithRootMountPoint(t *testing.T) {
	// Test with mount point at root level
	tempDir, err := os.MkdirTemp("", "overlayfs-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	mockDir := mockfs.NewDirectory()
	mountPoint := tempDir // Mount point is the temp dir itself

	overlayFS, err := New(mockDir, mountPoint)
	if err != nil {
		t.Fatalf("Failed to create overlay filesystem: %v", err)
	}

	// Verify overlay directory is parallel to mount point
	expectedOverlayDir := filepath.Join(filepath.Dir(tempDir), "."+filepath.Base(tempDir)+".overlay")
	if overlayFS.GetOverlayPath() != expectedOverlayDir {
		t.Errorf("Expected overlay directory to be %s, got %s", expectedOverlayDir, overlayFS.GetOverlayPath())
	}
}

//nolint:gocyclo // Test function with complex setup
func TestOverlayFileOperations(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "overlayfs-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create mock base directory with some files
	mockDir := mockfs.NewDirectory()
	mockDir.AddFile("existing.txt", []byte("base content"), 0o644)
	mockDir.AddFile("to-be-deleted.txt", []byte("will be deleted"), 0o644)

	mountPoint := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		t.Fatalf("Failed to create mount point: %v", err)
	}

	overlayFS, err := New(mockDir, mountPoint)
	if err != nil {
		t.Fatalf("Failed to create overlay filesystem: %v", err)
	}

	ctx := context.Background()

	// Test 1: Read existing file from snapshot
	entry, err := overlayFS.GetEntry(ctx, "existing.txt")
	if err != nil {
		t.Fatalf("Failed to get existing file: %v", err)
	}

	if entry.Name() != "existing.txt" {
		t.Errorf("Expected file name 'existing.txt', got '%s'", entry.Name())
	}

	// Test 2: Create new file in overlay
	newContent := []byte("new overlay content")

	err = overlayFS.CreateFile("new-file.txt", newContent, 0o644)
	if err != nil {
		t.Fatalf("Failed to create overlay file: %v", err)
	}

	// Test 3: Read new file from overlay
	entry, err = overlayFS.GetEntry(ctx, "new-file.txt")
	if err != nil {
		t.Fatalf("Failed to get overlay file: %v", err)
	}

	if entry.Name() != "new-file.txt" {
		t.Errorf("Expected file name 'new-file.txt', got '%s'", entry.Name())
	}

	// Test 4: Read content of overlay file
	file, ok := entry.(fs.File)
	if !ok {
		t.Fatalf("Expected entry to be a file")
	}

	reader, err := file.Open(ctx)
	if err != nil {
		t.Fatalf("Failed to open overlay file: %v", err)
	}

	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read overlay file: %v", err)
	}

	if !bytes.Equal(content, newContent) {
		t.Errorf("Expected content '%s', got '%s'", string(newContent), string(content))
	}

	// Test 5: Override existing file
	overrideContent := []byte("overridden content")

	err = overlayFS.CreateFile("existing.txt", overrideContent, 0o644)
	if err != nil {
		t.Fatalf("Failed to override file: %v", err)
	}

	entry, err = overlayFS.GetEntry(ctx, "existing.txt")
	if err != nil {
		t.Fatalf("Failed to get overridden file: %v", err)
	}

	file, ok = entry.(fs.File)
	if !ok {
		t.Fatalf("Expected entry to be a file")
	}

	reader, err = file.Open(ctx)
	if err != nil {
		t.Fatalf("Failed to open overridden file: %v", err)
	}

	defer reader.Close()

	content, err = io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read overridden file: %v", err)
	}

	if !bytes.Equal(content, overrideContent) {
		t.Errorf("Expected overridden content '%s', got '%s'", string(overrideContent), string(content))
	}

	// Test 6: Delete file
	err = overlayFS.DeleteFile("to-be-deleted.txt")
	if err != nil {
		t.Fatalf("Failed to delete file: %v", err)
	}

	_, err = overlayFS.GetEntry(ctx, "to-be-deleted.txt")
	if !os.IsNotExist(err) {
		t.Errorf("Expected deleted file to not exist, got error: %v", err)
	}
}

//nolint:gocyclo // Test function with complex setup
func TestDirectoryListing(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "overlayfs-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create mock base directory with some files
	mockDir := mockfs.NewDirectory()
	mockDir.AddFile("base-file1.txt", []byte("content1"), 0o644)
	mockDir.AddFile("base-file2.txt", []byte("content2"), 0o644)
	mockDir.AddFile("will-be-deleted.txt", []byte("to be deleted"), 0o644)

	mountPoint := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		t.Fatalf("Failed to create mount point: %v", err)
	}

	overlayFS, err := New(mockDir, mountPoint)
	if err != nil {
		t.Fatalf("Failed to create overlay filesystem: %v", err)
	}

	ctx := context.Background()

	// Add files to overlay
	err = overlayFS.CreateFile("overlay-file.txt", []byte("overlay content"), 0o644)
	if err != nil {
		t.Fatalf("Failed to create overlay file: %v", err)
	}

	// Override one base file
	err = overlayFS.CreateFile("base-file1.txt", []byte("overridden content"), 0o644)
	if err != nil {
		t.Fatalf("Failed to override base file: %v", err)
	}

	// Delete one base file
	err = overlayFS.DeleteFile("will-be-deleted.txt")
	if err != nil {
		t.Fatalf("Failed to delete file: %v", err)
	}

	// Get root directory and list entries
	rootDir, err := overlayFS.GetRootDirectory(ctx)
	if err != nil {
		t.Fatalf("Failed to get root directory: %v", err)
	}

	entries := make(map[string]fs.Entry)

	err = fs.IterateEntries(ctx, rootDir, func(ctx context.Context, entry fs.Entry) error {
		entries[entry.Name()] = entry
		return nil
	})
	if err != nil {
		t.Fatalf("Failed to iterate entries: %v", err)
	}

	// Verify expected files are present
	expectedFiles := []string{"base-file1.txt", "base-file2.txt", "overlay-file.txt"}
	for _, expectedFile := range expectedFiles {
		if _, exists := entries[expectedFile]; !exists {
			t.Errorf("Expected file '%s' not found in directory listing", expectedFile)
		}
	}

	// Verify deleted file is not present
	if _, exists := entries["will-be-deleted.txt"]; exists {
		t.Errorf("Deleted file 'will-be-deleted.txt' should not be in directory listing")
	}

	// Verify override worked
	overriddenEntry := entries["base-file1.txt"]

	file, ok := overriddenEntry.(fs.File)
	if !ok {
		t.Fatalf("Expected entry to be a file")
	}

	reader, err := file.Open(ctx)
	if err != nil {
		t.Fatalf("Failed to open overridden file: %v", err)
	}

	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read overridden file: %v", err)
	}

	if string(content) != "overridden content" {
		t.Errorf("Expected overridden content, got '%s'", string(content))
	}
}

func TestNestedDirectories(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "overlayfs-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create mock base directory with nested structure
	mockDir := mockfs.NewDirectory()
	subDir := mockDir.AddDir("subdir", 0o755)
	subDir.AddFile("nested-file.txt", []byte("nested content"), 0o644)

	mountPoint := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		t.Fatalf("Failed to create mount point: %v", err)
	}

	overlayFS, err := New(mockDir, mountPoint)
	if err != nil {
		t.Fatalf("Failed to create overlay filesystem: %v", err)
	}

	ctx := context.Background()

	// Test accessing nested file from base
	entry, err := overlayFS.GetEntry(ctx, "subdir/nested-file.txt")
	if err != nil {
		t.Fatalf("Failed to get nested file: %v", err)
	}

	if entry.Name() != "nested-file.txt" {
		t.Errorf("Expected file name 'nested-file.txt', got '%s'", entry.Name())
	}

	// Test creating nested file in overlay
	err = overlayFS.CreateFile("subdir/overlay-nested.txt", []byte("overlay nested content"), 0o644)
	if err != nil {
		t.Fatalf("Failed to create nested overlay file: %v", err)
	}

	entry, err = overlayFS.GetEntry(ctx, "subdir/overlay-nested.txt")
	if err != nil {
		t.Fatalf("Failed to get nested overlay file: %v", err)
	}

	nestedFile, ok := entry.(fs.File)
	if !ok {
		t.Fatalf("Expected entry to be a file")
	}

	reader, err := nestedFile.Open(ctx)
	if err != nil {
		t.Fatalf("Failed to open nested overlay file: %v", err)
	}

	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read nested overlay file: %v", err)
	}

	if string(content) != "overlay nested content" {
		t.Errorf("Expected nested overlay content, got '%s'", string(content))
	}
}

func TestCreateDirectory(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "overlayfs-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	mockDir := mockfs.NewDirectory()
	mountPoint := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		t.Fatalf("Failed to create mount point: %v", err)
	}

	overlayFS, err := New(mockDir, mountPoint)
	if err != nil {
		t.Fatalf("Failed to create overlay filesystem: %v", err)
	}

	// Test creating directory
	err = overlayFS.CreateDirectory("newdir", 0o755)
	if err != nil {
		t.Fatalf("Failed to create directory: %v", err)
	}

	// Verify directory was created in overlay
	expectedDirPath := filepath.Join(overlayFS.writtenDir, "newdir")
	if stat, err := os.Stat(expectedDirPath); err != nil {
		t.Fatalf("Expected directory not found: %v", err)
	} else if !stat.IsDir() {
		t.Errorf("Expected path to be a directory")
	}

	// Test nested directory creation
	err = overlayFS.CreateDirectory("parent/child", 0o755)
	if err != nil {
		t.Fatalf("Failed to create nested directory: %v", err)
	}

	expectedNestedPath := filepath.Join(overlayFS.writtenDir, "parent/child")
	if stat, err := os.Stat(expectedNestedPath); err != nil {
		t.Fatalf("Expected nested directory not found: %v", err)
	} else if !stat.IsDir() {
		t.Errorf("Expected nested path to be a directory")
	}
}

func TestGetOverlayFilePath(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "overlayfs-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	mockDir := mockfs.NewDirectory()
	mountPoint := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		t.Fatalf("Failed to create mount point: %v", err)
	}

	overlayFS, err := New(mockDir, mountPoint)
	if err != nil {
		t.Fatalf("Failed to create overlay filesystem: %v", err)
	}

	// Test getting overlay path for root file
	path := overlayFS.getOverlayFilePath("test.txt")
	expected := filepath.Join(overlayFS.writtenDir, "test.txt")
	if path != expected {
		t.Errorf("Expected path %s, got %s", expected, path)
	}

	// Test getting overlay path for nested file
	nestedPath := overlayFS.getOverlayFilePath("dir/nested/file.txt")
	expectedNested := filepath.Join(overlayFS.writtenDir, "dir/nested/file.txt")
	if nestedPath != expectedNested {
		t.Errorf("Expected nested path %s, got %s", expectedNested, nestedPath)
	}
}
