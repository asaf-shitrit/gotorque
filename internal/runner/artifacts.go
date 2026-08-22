// Package runner executes a configured CLI workload inside a campaign sandbox.
package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ArtifactStore persists immutable, content-addressed execution evidence.
// It is intentionally file based so a later object-store adapter can implement
// the same contract without changing run semantics.
type ArtifactStore struct{ Root string }

func NewArtifactStore(root string) (*ArtifactStore, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("artifact root must be absolute")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &ArtifactStore{Root: root}, nil
}

func (s *ArtifactStore) Put(name string, content []byte) (id, path string, err error) {
	if s == nil {
		return "", "", fmt.Errorf("artifact store is required")
	}
	if filepath.Base(name) != name {
		return "", "", fmt.Errorf("artifact name must not contain a path")
	}
	digest := Digest(content)
	id = digest
	path = filepath.Join(s.Root, digest+"-"+name)
	if err = os.WriteFile(path, content, 0o600); err != nil {
		return "", "", err
	}
	return id, path, nil
}

func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func DigestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SnapshotFiles captures regular output files below root. Input files should
// live in a different directory and are therefore never included.
func (s *ArtifactStore) SnapshotFiles(root string) (map[string]string, error) {
	if s == nil {
		return nil, fmt.Errorf("artifact store is required")
	}
	files := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	artifacts := make(map[string]string, len(files))
	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		_, stored, err := s.Put("file-"+safeName(rel), data)
		if err != nil {
			return nil, err
		}
		artifacts["output:"+filepath.ToSlash(rel)] = stored
	}
	return artifacts, nil
}

func safeName(name string) string {
	name = strings.ReplaceAll(filepath.ToSlash(name), "/", "_")
	return strings.ReplaceAll(name, "..", "_")
}
