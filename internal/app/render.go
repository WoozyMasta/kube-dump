// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"
	"github.com/woozymasta/kube-dump/v2/internal/archive"
	"github.com/woozymasta/kube-dump/v2/internal/compress"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/keyring"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"github.com/woozymasta/kube-dump/v2/internal/state/codec"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	maxRenderedMetadataBytes = 1 << 20
	maxRenderedResourceBytes = 64 << 20
)

// Execute reads canonical YAML files from a local directory.
func (c *ResourceCatDirCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("resource input is required")
	}

	info, err := os.Stat(c.Path)
	if err != nil {
		return fmt.Errorf("inspect resource input: %w", err)
	}
	if !info.IsDir() {
		return errors.New("resource directory source must be a directory")
	}

	return writeDirectoryOutput(c.context(), c.Path, c.IdentityOptions, c.output)
}

// Execute reads applyable YAML from a local compressed archive.
// Archive entries are consumed directly so only one resource is buffered.
func (c *ResourceCatArchiveCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("resource output is required")
	}

	return catArchiveFile(c.context(), c.Path, c.IdentityOptions, c.output)
}

// catArchiveFile opens and reads one local archive.
func catArchiveFile(ctx context.Context, path string, identityOptions IdentityOptions, output io.Writer) error {
	algorithm, encrypted, err := archiveAlgorithm(path)
	if err != nil {
		return err
	}

	input, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open resource archive: %w", err)
	}
	defer func() { _ = input.Close() }()

	var source io.Reader = input
	var identities []age.Identity
	identities, err = loadArchiveIdentities(identityOptions, encrypted)
	if err != nil {
		return err
	}

	if encrypted {
		source, err = agecrypto.OpenDecryptReader(input, identities)
		if err != nil {
			return fmt.Errorf("open encrypted archive: %w", err)
		}
	}

	return writeArchiveOutput(ctx, source, algorithm, identities, identityOptions, output)
}

// loadArchiveIdentities loads age identities only when the archive is wrapped.
func loadArchiveIdentities(identityOptions IdentityOptions, encrypted bool) ([]age.Identity, error) {
	if !encrypted {
		return nil, nil
	}

	identities, err := loadConfiguredIdentities(identityOptions)
	if err != nil {
		return nil, fmt.Errorf("load archive identities: %w", err)
	}

	return identities, nil
}

// Execute reads the current Git worktree
// with the same canonical directory reader used by resource cat dir.
// Git history is intentionally outside the YAML output contract.
func (c *ResourceCatGitCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("resource input is required")
	}

	return writeDirectoryOutput(c.context(), c.Path, c.IdentityOptions, c.output)
}

// Execute reads YAML objects from an S3 prefix in stable key order.
func (c *ResourceCatS3Command) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("resource input is required")
	}

	metadataStore, store, err := openCurrentS3ResourceStores(c.context(), c.S3Destination)
	if err != nil {
		return fmt.Errorf("open S3 source: %w", err)
	}

	return writeS3Output(c.context(), store, metadataStore, c.IdentityOptions, c.output)
}

// Execute reads one compressed resource archive stored in S3.
func (c *ResourceCatArchiveS3Command) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("resource output is required")
	}

	algorithm, encrypted, err := archiveAlgorithm(c.URI)
	if err != nil {
		return err
	}

	identities, err := loadArchiveIdentities(c.IdentityOptions, encrypted)
	if err != nil {
		return err
	}

	store, err := newS3Store(c.context(), c.S3Destination)
	if err != nil {
		return fmt.Errorf("open S3 archive source: %w", err)
	}

	body, err := store.OpenObject(c.context(), store.ObjectKey())
	if err != nil {
		return fmt.Errorf("open S3 resource archive: %w", err)
	}
	defer func() { _ = body.Close() }()

	return writeArchiveOutput(c.context(), body, algorithm, identities, c.IdentityOptions, c.output)
}

// writeDirectoryOutput writes the canonical directory in stable order.
func writeDirectoryOutput(
	ctx context.Context,
	root string,
	identityOptions IdentityOptions,
	output io.Writer,
) error {
	resourceDirectory := resourceRoot(root)
	if err := recoverResourcePublication(resourceDirectory); err != nil {
		return fmt.Errorf("recover resource publication: %w", err)
	}
	paths, err := resourcePaths(ctx, resourceDirectory)
	if err != nil {
		return fmt.Errorf("scan resource directory: %w", err)
	}

	identities, err := loadCatIdentities(identityOptions, false)
	if err != nil {
		return err
	}

	var keys fieldcrypto.SIVKeyResolver
	for index, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read resource file %q: %w", path, err)
		}

		value, err := codec.Unmarshal(data)
		if err != nil {
			return fmt.Errorf("decode resource file %q: %w", path, err)
		}

		object, err := objectFromResourcePath(resourceDirectory, path, value)
		if err != nil {
			return fmt.Errorf("identify resource file %q: %w", path, err)
		}

		if fieldcrypto.ContainsSIV(object) && keys == nil {
			keys, err = keyring.Open(root, nil, identities)
			if err != nil {
				return fmt.Errorf("open AES-SIV keyring: %w", err)
			}
		}

		if index > 0 {
			if _, err := io.WriteString(output, "---\n"); err != nil {
				return fmt.Errorf("write YAML document separator: %w", err)
			}
		}

		if err := writeCatObject(object, identities, keys, output); err != nil {
			return fmt.Errorf("read resource %q: %w", object.Identity.Key(), err)
		}
	}

	return nil
}

// writeArchiveOutput writes canonical YAML entries directly from an archive.
// Keyring files are read before resource entries and kept only in memory
// so AES-SIV restoration does not require extracting the archive to disk.
func writeArchiveOutput(
	ctx context.Context,
	source io.Reader,
	algorithm compress.Algorithm,
	archiveIdentities []age.Identity,
	fieldIdentityOptions IdentityOptions,
	output io.Writer,
) error {
	identities := archiveIdentities
	if len(identities) == 0 && len(fieldIdentityOptions.IdentityFiles) > 0 {
		loaded, err := loadConfiguredIdentities(fieldIdentityOptions)
		if err != nil {
			return fmt.Errorf("load resource identities: %w", err)
		}

		identities = loaded
	}

	var metadata []byte
	var keys fieldcrypto.SIVKeyResolver
	envelopes := make(map[string][]byte)
	objects := 0

	// Archive metadata and key envelopes are transport entries, not Kubernetes documents.
	// Read them first so a later encrypted object can initialize the AES-SIV resolver
	// without extracting the archive to a temporary directory.
	err := archive.ReadEntries(source, algorithm, func(entry archive.ReaderEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Dir {
			return nil
		}

		if entry.Path == ".kube-dump/crypto/metadata.yaml" {
			data, err := readLimited(entry.Reader, maxRenderedMetadataBytes)
			if err != nil {
				return fmt.Errorf("read archive entry %q: %w", entry.Path, err)
			}

			metadata = data
			return nil
		}

		if strings.HasPrefix(entry.Path, ".kube-dump/crypto/keys/") {
			data, err := readLimited(entry.Reader, maxRenderedMetadataBytes)
			if err != nil {
				return fmt.Errorf("read archive entry %q: %w", entry.Path, err)
			}

			envelopes[strings.TrimPrefix(entry.Path, ".kube-dump/crypto/")] = data
			return nil
		}

		canonicalPath, ok := canonicalResourcePath(entry.Path)
		if !ok {
			return nil
		}

		data, err := readLimited(entry.Reader, maxRenderedResourceBytes)
		if err != nil {
			return fmt.Errorf("read archive entry %q: %w", entry.Path, err)
		}

		value, err := codec.Unmarshal(data)
		if err != nil {
			return fmt.Errorf("decode archive resource %q: %w", entry.Path, err)
		}

		object, err := objectFromCanonicalPath(canonicalPath, value)
		if err != nil {
			return fmt.Errorf("identify archive resource %q: %w", entry.Path, err)
		}

		// AES-SIV keyring loading is lazy:
		// plain and age-encrypted objects do not require the keyring or an identity file.
		if fieldcrypto.ContainsSIV(object) && keys == nil {
			keys, err = keyring.OpenData(metadata, envelopes, identities)
			if err != nil {
				return fmt.Errorf("open AES-SIV keyring: %w", err)
			}
		}

		if objects > 0 {
			if _, err := io.WriteString(output, "---\n"); err != nil {
				return fmt.Errorf("write YAML document separator: %w", err)
			}
		}

		if err := writeCatObject(object, identities, keys, output); err != nil {
			return fmt.Errorf("read archive resource %q: %w", object.Identity.Key(), err)
		}
		objects++

		return nil
	})
	if err != nil {
		return fmt.Errorf("read resource archive: %w", err)
	}

	return nil
}

// writeS3Output writes resource files from an S3 prefix one object at a time.
// The stable key order ensures output does not depend on S3 listing order.
func writeS3Output(
	ctx context.Context,
	store *export.S3Store,
	metadataStore *export.S3Store,
	identityOptions IdentityOptions,
	output io.Writer,
) error {
	identities, err := loadCatIdentities(identityOptions, false)
	if err != nil {
		return err
	}

	var metadata []byte
	var keys fieldcrypto.SIVKeyResolver
	envelopes := make(map[string][]byte)
	objects := 0

	if metadataStore != nil {
		// S3 stores metadata and resource files under separate prefixes.
		// The metadata pass collects the keyring before the resource pass starts.
		if err := metadataStore.ForEachStoredFileStream(ctx, func(key string, reader io.Reader) error {
			relative := strings.TrimPrefix(strings.TrimPrefix(key, metadataStore.ObjectKey()), "/")
			if relative == "metadata.yaml" {
				data, err := readLimited(reader, maxRenderedMetadataBytes)
				if err != nil {
					return fmt.Errorf("read S3 metadata %q: %w", key, err)
				}

				metadata = data
				return nil
			}

			if strings.HasPrefix(relative, "keys/") {
				data, err := readLimited(reader, maxRenderedMetadataBytes)
				if err != nil {
					return fmt.Errorf("read S3 metadata %q: %w", key, err)
				}

				envelopes[relative] = data
			}

			return nil
		}); err != nil {
			return fmt.Errorf("read S3 resource metadata: %w", err)
		}
	}

	// The S3 backend provides a stable listing order,
	// so documents can be streamed directly while preserving deterministic multi-document output.
	if err := store.ForEachStoredFileStream(ctx, func(key string, reader io.Reader) error {
		relative := strings.TrimPrefix(strings.TrimPrefix(key, store.ObjectKey()), "/")
		canonicalPath, ok := canonicalResourcePath(relative)
		if !ok {
			return nil
		}

		data, err := readLimited(reader, maxRenderedResourceBytes)
		if err != nil {
			return fmt.Errorf("read S3 resource %q: %w", key, err)
		}

		value, err := codec.Unmarshal(data)
		if err != nil {
			return fmt.Errorf("decode S3 resource %q: %w", key, err)
		}

		object, err := objectFromCanonicalPath(canonicalPath, value)
		if err != nil {
			return fmt.Errorf("identify S3 resource %q: %w", key, err)
		}

		// Open the resolver only when the first AES-SIV object requires it;
		// this keeps plain resource exports independent of the keyring files.
		if fieldcrypto.ContainsSIV(object) && keys == nil {
			keys, err = keyring.OpenData(metadata, envelopes, identities)
			if err != nil {
				return fmt.Errorf("open AES-SIV keyring: %w", err)
			}
		}

		if objects > 0 {
			if _, err := io.WriteString(output, "---\n"); err != nil {
				return fmt.Errorf("write YAML document separator: %w", err)
			}
		}

		if err := writeCatObject(object, identities, keys, output); err != nil {
			return fmt.Errorf("read S3 resource %q: %w", key, err)
		}
		objects++

		return nil
	}); err != nil {
		return fmt.Errorf("read S3 resources: %w", err)
	}

	return nil
}

// readLimited buffers a small metadata or resource document
// and rejects an oversized object before the caller can parse or publish it.
func readLimited(source io.Reader, limit int64) ([]byte, error) {
	if source == nil || limit < 0 {
		return nil, errors.New("bounded reader arguments are invalid")
	}

	data, err := io.ReadAll(io.LimitReader(source, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("object exceeds %d-byte limit", limit)
	}

	return data, nil
}

// writeCatObject restores markers and writes one canonical YAML document.
func writeCatObject(
	object state.Object,
	identities []age.Identity,
	keys fieldcrypto.SIVKeyResolver,
	output io.Writer,
) error {
	if fieldcrypto.ContainsTaggedData(object) {
		var err error
		object, err = fieldcrypto.RestoreFields(object, identities, keys)
		if err != nil {
			return err
		}
	}

	data, err := codec.Marshal(object)
	if err != nil {
		return err
	}
	if _, err := output.Write(data); err != nil {
		return err
	}

	return nil
}

// resourcePaths returns canonical YAML files while excluding hidden transport metadata
// such as the repository keyring from Kubernetes output.
func resourcePaths(ctx context.Context, root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		if state.IsCanonicalObjectPath(filepath.ToSlash(relative)) {
			paths = append(paths, path)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(paths)
	return paths, nil
}

// canonicalResourcePath normalizes a path from either a shared capture tree
// or the resource subtree to the canonical resource path used by state.
func canonicalResourcePath(relative string) (string, bool) {
	relative = filepath.ToSlash(relative)
	if after, ok := strings.CutPrefix(relative, resourceLayoutDirectory+"/"); ok {
		relative = after
	}

	return relative, state.IsCanonicalObjectPath(relative)
}

// objectFromResourcePath reconstructs the state identity needed for AES-SIV AAD.
func objectFromResourcePath(root, path string, value *unstructured.Unstructured) (state.Object, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return state.Object{}, err
	}

	return objectFromCanonicalPath(filepath.ToSlash(relative), value)
}

// objectFromCanonicalPath reconstructs identity from a canonical resource path.
func objectFromCanonicalPath(relative string, value *unstructured.Unstructured) (state.Object, error) {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 5 {
		return state.Object{},
			errors.New("canonical resource path must be group/version/resource/namespace/name.yaml")
	}

	group, err := state.DecodePathSegment(parts[0])
	if err != nil {
		return state.Object{}, err
	}
	if group == "core" {
		group = ""
	}

	version, err := state.DecodePathSegment(parts[1])
	if err != nil {
		return state.Object{}, err
	}

	resource, err := state.DecodePathSegment(parts[2])
	if err != nil {
		return state.Object{}, err
	}

	namespace, err := state.DecodePathSegment(parts[3])
	if err != nil {
		return state.Object{}, err
	}
	if namespace == "_cluster" {
		namespace = ""
	}

	name, err := state.DecodePathSegment(strings.TrimSuffix(parts[4], ".yaml"))
	if err != nil {
		return state.Object{}, err
	}

	return state.NewObject(state.Identity{
		Group:     group,
		Version:   version,
		Resource:  resource,
		Kind:      value.GetKind(),
		Namespace: namespace,
		Name:      name,
	}, value)
}

// loadCatIdentities avoids requiring identity files for plaintext resources.
func loadCatIdentities(options IdentityOptions, encrypted bool) ([]age.Identity, error) {
	if len(options.IdentityFiles) == 0 {
		if encrypted {
			return nil, errors.New("encrypted resource fields require age identity files")
		}

		return nil, nil
	}

	return loadConfiguredIdentities(options)
}

// archiveAlgorithm detects the codec and age envelope from an archive suffix.
func archiveAlgorithm(path string) (compress.Algorithm, bool, error) {
	encrypted := strings.HasSuffix(path, ".age")
	if encrypted {
		path = strings.TrimSuffix(path, ".age")
	}

	switch filepath.Ext(path) {
	case ".gz":
		return compress.Gzip, encrypted, nil

	case ".zst":
		return compress.Zstandard, encrypted, nil

	default:
		return "", false, fmt.Errorf(
			"unsupported backup archive %q: use .tar.gz, .tar.gz.age, .tar.zst, or .tar.zst.age",
			path,
		)
	}
}
