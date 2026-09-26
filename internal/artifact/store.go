// Package artifact provides sandboxed, integrity-verified file storage for task inputs and outputs.
package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidPath  = errors.New("invalid or illegal file path")
	ErrChecksumFail = errors.New("checksum mismatch")
	ErrNotFound     = errors.New("artifact not found")
	ErrClosed       = errors.New("artifact store closed")
)

// FileInfo holds metadata about an artifact in the store.
type FileInfo struct {
	TaskID    string    `json:"task_id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store manages file artifacts within a constrained directory hierarchy using os.Root.
type Store struct {
	rootDir string
	root    *os.Root
	mu      sync.RWMutex
	closed  bool
}

// Open creates or opens the artifact store rooted at dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: root directory cannot be empty", ErrInvalidPath)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact directory: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact directory: %w", err)
	}

	root, err := os.OpenRoot(absDir)
	if err != nil {
		return nil, fmt.Errorf("open root sandbox: %w", err)
	}

	return &Store{
		rootDir: absDir,
		root:    root,
	}, nil
}

// sanitizeSegment validates that a path element contains no path traversal or separators.
func sanitizeSegment(seg string) error {
	if seg == "" || seg == "." || seg == ".." {
		return ErrInvalidPath
	}
	if strings.ContainsAny(seg, "/\\:\x00") {
		return fmt.Errorf("%w: invalid characters in segment %q", ErrInvalidPath, seg)
	}
	return nil
}

// Save streams content into a task-scoped artifact. Writes to a temporary file first,
// computes SHA-256, verifies expected hash if provided, and atomically renames the file into place.
func (s *Store) Save(ctx context.Context, taskID, filename string, r io.Reader, expectedSHA256 string) (FileInfo, error) {
	if err := sanitizeSegment(taskID); err != nil {
		return FileInfo{}, fmt.Errorf("invalid task_id: %w", err)
	}
	if err := sanitizeSegment(filename); err != nil {
		return FileInfo{}, fmt.Errorf("invalid filename: %w", err)
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return FileInfo{}, ErrClosed
	}
	root := s.root
	s.mu.RUnlock()

	// Ensure task subdirectory exists
	if err := root.MkdirAll(taskID, 0o700); err != nil {
		return FileInfo{}, fmt.Errorf("create task artifact directory: %w", err)
	}

	// Generate temporary file name inside the task directory
	randSuffix := make([]byte, 8)
	_, _ = rand.Read(randSuffix)
	tmpRelPath := filepath.Join(taskID, fmt.Sprintf(".tmp-%s-%x", filename, randSuffix))
	finalRelPath := filepath.Join(taskID, filename)

	tmpFile, err := root.OpenFile(tmpRelPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return FileInfo{}, fmt.Errorf("create temporary artifact file: %w", err)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(tmpFile, hasher)

	written, copyErr := io.Copy(writer, r)
	closeErr := tmpFile.Close()

	if copyErr != nil || closeErr != nil {
		_ = root.Remove(tmpRelPath)
		if copyErr != nil {
			return FileInfo{}, fmt.Errorf("write artifact: %w", copyErr)
		}
		return FileInfo{}, fmt.Errorf("close temporary artifact file: %w", closeErr)
	}

	computedSHA := hex.EncodeToString(hasher.Sum(nil))
	if expectedSHA256 != "" && !strings.EqualFold(expectedSHA256, computedSHA) {
		_ = root.Remove(tmpRelPath)
		return FileInfo{}, fmt.Errorf("%w: expected %s, computed %s", ErrChecksumFail, expectedSHA256, computedSHA)
	}

	// Atomic rename to final path within the root
	if err := root.Rename(tmpRelPath, finalRelPath); err != nil {
		_ = root.Remove(tmpRelPath)
		return FileInfo{}, fmt.Errorf("atomic publish artifact: %w", err)
	}

	fi, err := root.Stat(finalRelPath)
	modTime := time.Now().UTC()
	if err == nil {
		modTime = fi.ModTime().UTC()
	}

	return FileInfo{
		TaskID:    taskID,
		Name:      filename,
		Size:      written,
		SHA256:    computedSHA,
		UpdatedAt: modTime,
	}, nil
}

// Open retrieves an open file handle and its metadata within the root sandbox.
// The caller is responsible for closing the returned file.
func (s *Store) Open(ctx context.Context, taskID, filename string) (*os.File, FileInfo, error) {
	if err := sanitizeSegment(taskID); err != nil {
		return nil, FileInfo{}, fmt.Errorf("invalid task_id: %w", err)
	}
	if err := sanitizeSegment(filename); err != nil {
		return nil, FileInfo{}, fmt.Errorf("invalid filename: %w", err)
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, FileInfo{}, ErrClosed
	}
	root := s.root
	s.mu.RUnlock()

	relPath := filepath.Join(taskID, filename)
	stat, err := root.Stat(relPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, FileInfo{}, ErrNotFound
		}
		return nil, FileInfo{}, fmt.Errorf("stat artifact: %w", err)
	}
	if stat.IsDir() {
		return nil, FileInfo{}, fmt.Errorf("%w: path is a directory", ErrInvalidPath)
	}

	f, err := root.Open(relPath)
	if err != nil {
		return nil, FileInfo{}, fmt.Errorf("open artifact: %w", err)
	}

	return f, FileInfo{
		TaskID:    taskID,
		Name:      filename,
		Size:      stat.Size(),
		UpdatedAt: stat.ModTime().UTC(),
	}, nil
}

// List returns all non-temporary files associated with a task.
func (s *Store) List(ctx context.Context, taskID string) ([]FileInfo, error) {
	if err := sanitizeSegment(taskID); err != nil {
		return nil, fmt.Errorf("invalid task_id: %w", err)
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, ErrClosed
	}
	root := s.root
	s.mu.RUnlock()

	f, err := root.Open(taskID)
	if err != nil {
		if os.IsNotExist(err) {
			return []FileInfo{}, nil
		}
		return nil, fmt.Errorf("open task artifact directory: %w", err)
	}
	defer f.Close()

	entries, err := f.Readdir(-1)
	if err != nil {
		return nil, fmt.Errorf("read task artifact directory: %w", err)
	}

	var results []FileInfo
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".tmp-") {
			continue
		}
		results = append(results, FileInfo{
			TaskID:    taskID,
			Name:      entry.Name(),
			Size:      entry.Size(),
			UpdatedAt: entry.ModTime().UTC(),
		})
	}
	return results, nil
}

// Delete removes an artifact from a task directory.
func (s *Store) Delete(ctx context.Context, taskID, filename string) error {
	if err := sanitizeSegment(taskID); err != nil {
		return fmt.Errorf("invalid task_id: %w", err)
	}
	if err := sanitizeSegment(filename); err != nil {
		return fmt.Errorf("invalid filename: %w", err)
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return ErrClosed
	}
	root := s.root
	s.mu.RUnlock()

	relPath := filepath.Join(taskID, filename)
	if err := root.Remove(relPath); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("delete artifact: %w", err)
	}
	return nil
}

// Close closes the underlying root sandbox directory.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true
	return s.root.Close()
}
