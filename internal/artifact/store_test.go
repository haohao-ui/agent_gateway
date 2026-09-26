package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestArtifactStore_SaveAndOpen(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	taskID := "task-101"
	filename := "result.txt"
	content := []byte("Hello, sandboxed artifact world!")

	hasher := sha256.New()
	hasher.Write(content)
	expectedSHA := hex.EncodeToString(hasher.Sum(nil))

	// 1. Save artifact
	info, err := store.Save(ctx, taskID, filename, bytes.NewReader(content), expectedSHA)
	if err != nil {
		t.Fatalf("save artifact: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), info.Size)
	}
	if info.SHA256 != expectedSHA {
		t.Errorf("expected sha %s, got %s", expectedSHA, info.SHA256)
	}

	// 2. Open and verify content
	file, openInfo, err := store.Open(ctx, taskID, filename)
	if err != nil {
		t.Fatalf("open artifact: %v", err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Errorf("content mismatch: got %q, want %q", data, content)
	}
	if openInfo.Size != int64(len(content)) {
		t.Errorf("openInfo size mismatch: %d", openInfo.Size)
	}

	// 3. List
	items, err := store.List(ctx, taskID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(items) != 1 || items[0].Name != filename {
		t.Fatalf("unexpected list items: %+v", items)
	}

	// 4. Delete
	if err := store.Delete(ctx, taskID, filename); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	_, _, err = store.Open(ctx, taskID, filename)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after deletion, got %v", err)
	}
}

func TestArtifactStore_ChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	taskID := "task-bad-checksum"
	filename := "corrupt.bin"
	content := []byte("some content")

	badChecksum := "0000000000000000000000000000000000000000000000000000000000000000"

	_, err = store.Save(ctx, taskID, filename, bytes.NewReader(content), badChecksum)
	if !errors.Is(err, ErrChecksumFail) {
		t.Fatalf("expected ErrChecksumFail, got %v", err)
	}

	// Verify temporary file was deleted and final file does not exist
	finalPath := filepath.Join(dir, taskID, filename)
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Errorf("final file should not exist on checksum failure")
	}

	entries, _ := os.ReadDir(filepath.Join(dir, taskID))
	if len(entries) > 0 {
		t.Errorf("temporary file was not cleaned up: %+v", entries)
	}
}

func TestArtifactStore_PathTraversalBlocked(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	traversalCases := []struct {
		taskID   string
		filename string
	}{
		{"..", "file.txt"},
		{"task", "../file.txt"},
		{"task/sub", "file.txt"},
		{"task", "foo/bar.txt"},
		{"task", "foo\\bar.txt"},
		{".", "file.txt"},
		{"", "file.txt"},
		{"task", ""},
	}

	for _, tc := range traversalCases {
		_, err := store.Save(ctx, tc.taskID, tc.filename, bytes.NewReader([]byte("x")), "")
		if err == nil {
			t.Errorf("expected error for traversal case taskID=%q filename=%q, got nil", tc.taskID, tc.filename)
		}

		_, _, err = store.Open(ctx, tc.taskID, tc.filename)
		if err == nil {
			t.Errorf("expected error for open traversal case taskID=%q filename=%q, got nil", tc.taskID, tc.filename)
		}
	}
}

func TestArtifactStore_ConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	const numWorkers = 10
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for i := 0; i < numWorkers; i++ {
		go func(id int) {
			defer wg.Done()
			taskID := "concurrent-task"
			filename := filepath.Base(filepath.Join("task", string(rune('a'+id))+".txt"))
			content := []byte("worker content")

			info, err := store.Save(context.Background(), taskID, filename, bytes.NewReader(content), "")
			if err != nil {
				t.Errorf("worker %d save: %v", id, err)
				return
			}
			f, _, err := store.Open(context.Background(), taskID, filename)
			if err != nil {
				t.Errorf("worker %d open: %v", id, err)
				return
			}
			f.Close()
			_ = info
		}(i)
	}

	wg.Wait()

	items, err := store.List(context.Background(), "concurrent-task")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != numWorkers {
		t.Errorf("expected %d items, got %d", numWorkers, len(items))
	}
}
