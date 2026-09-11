// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/woozymasta/kube-dump/v2/internal/compress"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/pvc"
	goyaml "go.yaml.in/yaml/v3"
)

const (
	maxVolumeUploadResultBytes = 4000
	volumeUploadCleanupTimeout = 30 * time.Second
)

// volumeUploadStreamResult carries the producer result independently
// from the S3 consumer because either side of the pipe may report the first failure.
type volumeUploadStreamResult struct {
	err      error        // err is the producer-side export or encoding failure.
	metadata pvc.Metadata // metadata describes the completed uncompressed payload.
}

// volumeUploadResult is the bounded JSON protocol
// written to the Pod termination log after a direct upload.
type volumeUploadResult struct {
	Metadata *pvc.Metadata `json:"metadata,omitempty"` // Metadata is present only after a complete upload.
	Error    string        `json:"error,omitempty"`    // Error contains a bounded helper failure message.
}

// volumeCountWriter hashes and counts the uncompressed tar stream
// while it is forwarded to the codec pipeline.
type volumeCountWriter struct {
	writer io.Writer // writer receives the encoded stream bytes.
	bytes  int64     // bytes is the number of uncompressed payload bytes accepted by writer.
}

// volumeCountReader counts the uncompressed bytes consumed by a helper import.
type volumeCountReader struct {
	reader io.Reader // reader provides the decoded payload stream.
	bytes  int64     // bytes is the number of payload bytes returned to the importer.
}

// volumeDecodedStream owns both the codec reader and the remote response body.
type volumeDecodedStream struct {
	io.Reader
	closers []io.Closer
}

// Close releases the decoded stream and its underlying S3 response body.
func (s *volumeDecodedStream) Close() error {
	var closeErr error
	for _, closer := range slices.Backward(s.closers) {
		closeErr = errors.Join(closeErr, closer.Close())
	}

	return closeErr
}

// Read forwards decoded payload bytes and records the number consumed.
func (r *volumeCountReader) Read(data []byte) (int, error) {
	read, err := r.reader.Read(data)
	r.bytes += int64(read)

	return read, err
}

// Execute streams a safe tar archive of the mounted volume to the helper Pod's stdout.
// The command emits no logs so stdout remains a binary-only channel.
func (c *VolumeExportCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("volume export output is required")
	}

	return pvc.ExportVolume(c.context(), c.Positional.Path, c.output)
}

// Execute extracts the helper Pod's stdin archive into the mounted volume.
// The PVC agent validates every archive path and applies the requested policy.
func (c *VolumeImportCommand) Execute(_ []string) error {
	if c.input == nil {
		return errors.New("volume import input is required")
	}

	policy := pvc.ExistingPolicy(c.Positional.Existing)
	if err := policy.Validate(); err != nil {
		return err
	}

	return pvc.ImportVerifiedVolume(c.context(), c.Positional.Path, c.input, policy)
}

// Execute keeps the helper Pod alive until the parent process cancels it.
func (c *VolumeHoldCommand) Execute(_ []string) error {
	<-c.context().Done()
	return nil
}

// Execute exports a mounted filesystem, compresses and optionally encrypts it,
// then uploads the stream directly to S3.
//
// The command is a one-shot helper protocol:
// metadata is written only after the data object is complete,
// and the termination message is the result channel for the parent.
func (c *VolumeUploadCommand) Execute(_ []string) error {
	if c.Positional.URI == "" || c.Positional.Object == "" {
		return errors.New("S3 URI and object path are required")
	}
	if err := validateVolumeUploadObject(c.Positional.Object); err != nil {
		return err
	}
	if err := (compress.Config{Algorithm: c.Compression}).Validate(); err != nil {
		return fmt.Errorf("validate volume upload compression: %w", err)
	}
	if err := c.Strategy.Validate(); err != nil {
		return fmt.Errorf("validate volume upload strategy: %w", err)
	}

	recipients, err := parseRecipients(c.Recipients, nil, nil, nil)
	if err != nil {
		return c.writeUploadError(fmt.Errorf("parse volume upload recipients: %w", err))
	}

	store, err := export.NewS3Store(c.context(), export.S3Options{
		URI:      c.Positional.URI,
		Endpoint: c.Endpoint,
		Insecure: c.Insecure,
		Region:   c.Region,
	})
	if err != nil {
		return c.writeUploadError(fmt.Errorf("create volume upload S3 store: %w", err))
	}

	pipeReader, pipeWriter := io.Pipe()
	streamResult := make(chan volumeUploadStreamResult, 1)

	// The producer and S3 multipart uploader run concurrently through the pipe;
	// backpressure prevents the helper from buffering the whole PVC in memory.
	go func() {
		metadata, streamErr := writeVolumeUploadStream(
			c.context(),
			c.Positional.Path,
			pipeWriter,
			c.Compression,
			recipients,
			c.Strategy,
		)
		if streamErr != nil {
			_ = pipeWriter.CloseWithError(streamErr)
		} else {
			_ = pipeWriter.Close()
		}
		streamResult <- volumeUploadStreamResult{metadata: metadata, err: streamErr}
	}()

	dataKey := path.Join(
		store.ObjectKey(),
		c.Positional.Object,
		pvc.DataFilename(string(c.Compression), len(recipients) > 0),
	)
	dataObject := path.Join(
		c.Positional.Object,
		pvc.DataFilename(string(c.Compression), len(recipients) > 0),
	)

	metadataPublished := false
	cleanupUncommittedData := func(primary error) error {
		if primary == nil || metadataPublished {
			return primary
		}

		cleanupContext, cancel := context.WithTimeout(context.Background(), volumeUploadCleanupTimeout)
		defer cancel()
		if cleanupErr := store.DeleteObject(cleanupContext, dataObject); cleanupErr != nil {
			return errors.Join(primary, fmt.Errorf("cleanup incomplete volume upload: %w", cleanupErr))
		}

		return primary
	}

	// Start upload before the producer finishes.
	// io.Pipe provides bounded backpressure, so the helper never buffers a complete PVC in memory.
	uploadErr := store.PutReader(c.context(), dataKey, pipeReader, "application/octet-stream")
	if uploadErr != nil {
		_ = pipeReader.CloseWithError(uploadErr)
	}

	// Wait for both sides of the pipe.
	// The producer result is required before metadata can be published,
	// and any uploader error must be joined with it.
	stream := <-streamResult
	if uploadErr != nil || stream.err != nil {
		return c.writeUploadError(cleanupUncommittedData(errors.Join(uploadErr, stream.err)))
	}

	metadataData, err := goyaml.Marshal(stream.metadata)
	if err != nil {
		return c.writeUploadError(fmt.Errorf("encode volume upload metadata: %w", err))
	}

	if err := store.PutBytes(
		c.context(),
		path.Join(c.Positional.Object, pvc.MetadataFilename),
		metadataData,
		"application/yaml",
	); err != nil {
		return c.writeUploadError(cleanupUncommittedData(fmt.Errorf("write volume upload metadata: %w", err)))
	}
	metadataPublished = true

	return c.writeUploadResult(volumeUploadResult{Metadata: &stream.metadata})
}

// writeVolumeUploadStream produces the same portable payload and metadata
// as the client-mediated PVC strategy, but sends bytes to a non-seekable pipe.
func writeVolumeUploadStream(
	ctx context.Context,
	root string,
	destination io.Writer,
	compression compress.Algorithm,
	recipients []age.Recipient,
	strategy pvc.Strategy,
) (pvc.Metadata, error) {
	if destination == nil {
		return pvc.Metadata{}, errors.New("volume upload destination is required")
	}

	var encrypted io.WriteCloser
	streamDestination := destination
	if len(recipients) > 0 {
		var err error
		encrypted, err = agecrypto.NewEncryptWriter(destination, recipients, agecrypto.EncryptOptions{})
		if err != nil {
			return pvc.Metadata{}, fmt.Errorf("create volume upload encryptor: %w", err)
		}

		streamDestination = encrypted
	}

	compressor, err := compress.NewWriter(streamDestination, compress.Config{Algorithm: compression})
	if err != nil {
		if encrypted != nil {
			_ = encrypted.Close()
		}

		return pvc.Metadata{}, fmt.Errorf("create volume upload compressor: %w", err)
	}

	digest := sha256.New()
	count := &volumeCountWriter{writer: io.MultiWriter(compressor, digest)}
	exportErr := pvc.ExportVolume(ctx, root, count)
	closeErr := compressor.Close()
	if encrypted != nil {
		closeErr = errors.Join(closeErr, encrypted.Close())
	}
	if exportErr != nil {
		return pvc.Metadata{}, errors.Join(fmt.Errorf("export volume: %w", exportErr), closeErr)
	}
	if closeErr != nil {
		return pvc.Metadata{}, fmt.Errorf("close volume upload stream: %w", closeErr)
	}

	metadata := pvc.NewMetadata(strategy, string(compression))
	metadata.ContentSHA256 = hex.EncodeToString(digest.Sum(nil))
	metadata.SizeBytes = count.bytes
	metadata.Encrypted = len(recipients) > 0
	metadata.Portable = true

	return metadata, nil
}

// Write forwards data and updates the uncompressed payload size.
// Write forwards uncompressed payload bytes and records the number accepted.
func (w *volumeCountWriter) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	w.bytes += int64(written)
	return written, err
}

// validateVolumeUploadObject rejects keys that could escape the backup prefix
// or produce different object names on Windows and POSIX helper containers.
func validateVolumeUploadObject(object string) error {
	if object == "" || strings.ContainsRune(object, '\x00') {
		return errors.New("volume upload object is invalid")
	}

	normalized := strings.ReplaceAll(object, "\\", "/")
	clean := path.Clean(normalized)
	if clean == "." || clean != normalized || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("volume upload object must be a relative path without parent traversal: %q", object)
	}

	return nil
}

// Execute downloads one S3 archive, decrypts and decompresses it,
// validates it in a first pass, then imports the pinned second pass.
func (c *VolumeDownloadCommand) Execute(_ []string) error {
	if c.Positional.URI == "" || c.Positional.Object == "" {
		return errors.New("S3 URI and object path are required")
	}

	if err := validateVolumeUploadObject(c.Positional.Object); err != nil {
		return err
	}
	if err := (compress.Config{Algorithm: c.Compression}).Validate(); err != nil {
		return fmt.Errorf("validate volume download compression: %w", err)
	}
	if err := c.Positional.Existing.Validate(); err != nil {
		return err
	}
	if c.ExpectedSize < 0 {
		return errors.New("expected volume payload size must not be negative")
	}

	store, err := export.NewS3Store(c.context(), export.S3Options{
		URI:      c.Positional.URI,
		Endpoint: c.Endpoint,
		Insecure: c.Insecure,
		Region:   c.Region,
	})
	if err != nil {
		return fmt.Errorf("create volume download S3 store: %w", err)
	}

	var identities []age.Identity
	if c.Encrypted {
		identities, err = agecrypto.LoadIdentities(
			[]string{c.IdentityPath},
			agecrypto.IdentityOptions{Passphrase: c.IdentityPassphrase},
		)
		if err != nil {
			return fmt.Errorf("load volume download identity: %w", err)
		}
	}

	openDecoded := func(expected *export.ObjectIdentity) (*volumeDecodedStream, export.ObjectIdentity, error) {
		data, identity, err := store.OpenObjectWithIdentity(c.context(), c.Positional.Object, expected)
		if err != nil {
			return nil, export.ObjectIdentity{}, fmt.Errorf("open volume download S3 object: %w", err)
		}

		decrypted, err := agecrypto.MaybeDecryptReader(data, identities)
		if err != nil {
			_ = data.Close()
			return nil, export.ObjectIdentity{}, fmt.Errorf("decrypt volume download object: %w", err)
		}

		decoded, err := compress.NewReader(decrypted, c.Compression)
		if err != nil {
			_ = data.Close()
			return nil, export.ObjectIdentity{}, fmt.Errorf("open volume download compression: %w", err)
		}

		return &volumeDecodedStream{
			Reader:  decoded,
			closers: []io.Closer{data, decoded},
		}, identity, nil
	}

	// A complete validation pass must finish before import can mutate the target.
	validationStream, identity, err := openDecoded(nil)
	if err != nil {
		return err
	}

	stats, validationErr := pvc.ValidateVolumeArchive(c.context(), validationStream)
	closeErr := validationStream.Close()
	if validationErr != nil {
		return fmt.Errorf("validate volume download archive: %w", validationErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close volume download validation stream: %w", closeErr)
	}
	if err := validateVolumeDownloadStats(stats, c.ExpectedSize, c.ExpectedSHA256); err != nil {
		return err
	}

	decoded, _, err := openDecoded(&identity)
	if err != nil {
		return err
	}

	digest := sha256.New()
	count := &volumeCountReader{reader: io.TeeReader(decoded, digest)}
	importErr := pvc.ImportVerifiedVolume(c.context(), c.Positional.Path, count, c.Positional.Existing)
	closeErr = decoded.Close()
	if importErr != nil {
		return errors.Join(fmt.Errorf("import volume download archive: %w", importErr), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close volume download stream: %w", closeErr)
	}

	if err := validateVolumeDownloadStats(pvc.ArchiveStats{
		SizeBytes:     count.bytes,
		ContentSHA256: hex.EncodeToString(digest.Sum(nil)),
	}, c.ExpectedSize, c.ExpectedSHA256); err != nil {
		return err
	}

	return nil
}

// validateVolumeDownloadStats compares one decoded archive pass with helper expectations.
func validateVolumeDownloadStats(stats pvc.ArchiveStats, expectedSize int64, expectedSHA256 string) error {
	if stats.SizeBytes != expectedSize {
		return fmt.Errorf("volume download payload size %d does not match expected %d", stats.SizeBytes, expectedSize)
	}
	if expectedSHA256 != "" && stats.ContentSHA256 != expectedSHA256 {
		return errors.New("volume download payload checksum does not match expected SHA-256")
	}

	return nil
}

// writeUploadResult writes a compact result for the parent Pod watcher.
func (c *VolumeUploadCommand) writeUploadResult(result volumeUploadResult) error {
	data, err := marshalVolumeUploadResult(result)
	if err != nil {
		return fmt.Errorf("encode volume upload result: %w", err)
	}
	if err := os.WriteFile(c.Positional.Result, data, 0o600); err != nil {
		return fmt.Errorf("write volume upload result: %w", err)
	}

	return nil
}

// marshalVolumeUploadResult keeps the helper protocol parseable in a Pod termination log.
// Error text is truncated only when needed; successful metadata is never silently truncated.
func marshalVolumeUploadResult(result volumeUploadResult) ([]byte, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(data) <= maxVolumeUploadResultBytes {
		return data, nil
	}
	if result.Error == "" {
		return nil, fmt.Errorf("volume upload result exceeds %d bytes", maxVolumeUploadResultBytes)
	}

	const suffix = "... [truncated]"
	text := strings.ToValidUTF8(result.Error, "�")
	runes := []rune(text)
	best := suffix
	low, high := 0, len(runes)
	for low <= high {
		middle := low + (high-low)/2
		candidate := volumeUploadResult{Error: string(runes[:middle]) + suffix}
		encoded, marshalErr := json.Marshal(candidate)
		if marshalErr != nil {
			return nil, marshalErr
		}

		if len(encoded) <= maxVolumeUploadResultBytes {
			best = candidate.Error
			low = middle + 1
			continue
		}

		high = middle - 1
	}

	return json.Marshal(volumeUploadResult{Error: best})
}

// writeUploadError writes a bounded machine-readable failure before returning the same error,
// so a failed Pod remains diagnosable through its termination log.
func (c *VolumeUploadCommand) writeUploadError(uploadErr error) error {
	if uploadErr == nil {
		return errors.New("volume upload failed")
	}

	resultErr := c.writeUploadResult(volumeUploadResult{Error: uploadErr.Error()})
	return errors.Join(uploadErr, resultErr)
}
