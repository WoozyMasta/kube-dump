// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package export

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsretry "github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awss3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/fileutil"
	"github.com/woozymasta/kube-dump/v2/internal/retry"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"github.com/woozymasta/kube-dump/v2/internal/state/codec"
)

const contentDigestMetadata = "kube-dump-sha256"

const (
	s3MultipartPartSize      = 64 << 20
	maxBufferedS3ObjectBytes = 64 << 20
	maxListedS3Objects       = 1_000_000
	s3CleanupTimeout         = 30 * time.Second
)

// S3Options describes an S3 bucket and the optional S3-compatible endpoint.
type S3Options struct {
	// URI is the destination URI containing bucket and optional prefix.
	URI string
	// Endpoint overrides the provider API endpoint.
	Endpoint string
	// Region selects the S3 region.
	Region string
	// Bucket overrides the bucket parsed from URI.
	Bucket string
	// Prefix overrides the object prefix parsed from URI.
	Prefix string
	// AccessKey is the optional static access key.
	AccessKey string
	// SecretKey is the static secret key read by the caller from a file.
	SecretKey string
	// Insecure permits plaintext HTTP and loopback development endpoints.
	Insecure bool
}

// ValidateEndpoint checks the custom S3 endpoint before credentials are loaded.
// Plain HTTP and loopback endpoints require an explicit insecure opt-in.
func ValidateEndpoint(endpoint string, insecure bool) error {
	value := strings.TrimSpace(endpoint)
	if value == "" {
		return nil
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid S3 endpoint %q", endpoint)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("S3 endpoint %q must use http or https", endpoint)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("S3 endpoint %q must not contain credentials, query, or fragment", endpoint)
	}

	host := parsed.Hostname()
	loopback := strings.EqualFold(host, "localhost")
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	if (parsed.Scheme == "http" || loopback) && !insecure {
		return fmt.Errorf("S3 endpoint %q requires --s3-insecure", endpoint)
	}

	return nil
}

// S3Store reads and writes canonical resource files and archive objects under
// one S3 prefix.
type S3Store struct {
	// client performs S3 API operations.
	client *awss3.Client
	// bucket is the target bucket.
	bucket string
	// prefix is the canonical object prefix.
	prefix string
}

// progressReader reports bytes consumed by an S3 upload request.
type progressReader struct {
	reader   io.ReadSeeker
	progress func(int64)
}

// Read forwards source data and reports only bytes returned by the source.
func (r *progressReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if read > 0 {
		r.progress(int64(read))
	}

	return read, err
}

// Seek preserves the source seek contract required by the AWS SDK
// for checksum calculation when an S3 endpoint is accessed over plain HTTP.
func (r *progressReader) Seek(offset int64, whence int) (int64, error) {
	return r.reader.Seek(offset, whence)
}

// StoredObject describes one object below an S3 store prefix without reading
// its body. Size is supplied by ListObjectsV2 and is therefore suitable for
// inspection of large object trees.
type StoredObject struct {
	// LastModified is the server-provided modification time.
	LastModified time.Time
	// Key is the complete object key in the bucket.
	Key string
	// ETag is the server-provided entity tag from object listing.
	ETag string
	// Size is the object size in bytes.
	Size int64
}

// ObjectIdentity identifies one immutable S3 object version for replayed reads.
type ObjectIdentity struct {
	// ETag is the server-provided entity tag.
	ETag string
	// VersionID is the optional version identifier.
	VersionID string
	// Size is the response content length.
	Size int64
}

// Validate ensures an object identity contains a server-side version marker
// that can pin a repeated read to the same object bytes.
func (i ObjectIdentity) Validate() error {
	if i.ETag == "" && i.VersionID == "" {
		return errors.New("S3 object has neither ETag nor version ID")
	}

	return nil
}

// ObjectKey returns the configured prefix as an object key for archive output.
func (s *S3Store) ObjectKey() string {
	if s == nil {
		return ""
	}

	return s.prefix
}

// PutFile uploads one local archive as a single S3 object.
func (s *S3Store) PutFile(ctx context.Context, key, filename string) error {
	return s.PutFileWithProgress(ctx, key, filename, nil)
}

// ObjectExists checks one relative object key without downloading its body.
func (s *S3Store) ObjectExists(ctx context.Context, key string) (bool, error) {
	if s == nil || s.client == nil {
		return false, errors.New("S3 store is not initialized")
	}
	if ctx == nil || key == "" {
		return false, errors.New("S3 object key and context are required")
	}

	_, err := s.headObject(ctx, key)
	if err == nil {
		return true, nil
	}
	if isS3NotFound(err) {
		return false, nil
	}

	return false, fmt.Errorf("inspect S3 object %q: %w", key, err)
}

// HeadObjectIdentity returns the immutable identity advertised by one S3 object.
// Callers can use it to pin a later GET or conditional publication to the same object.
func (s *S3Store) HeadObjectIdentity(ctx context.Context, key string) (ObjectIdentity, error) {
	object, err := s.headObject(ctx, key)
	if err != nil {
		return ObjectIdentity{}, err
	}

	identity := ObjectIdentity{
		ETag:      aws.ToString(object.ETag),
		VersionID: aws.ToString(object.VersionId),
		Size:      aws.ToInt64(object.ContentLength),
	}
	if err := identity.Validate(); err != nil {
		return ObjectIdentity{}, fmt.Errorf("cannot pin S3 object %q: %w", key, err)
	}

	return identity, nil
}

// headObject reads metadata for one relative object without downloading its body.
func (s *S3Store) headObject(ctx context.Context, key string) (*awss3.HeadObjectOutput, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("S3 store is not initialized")
	}
	if ctx == nil || key == "" {
		return nil, errors.New("S3 object key and context are required")
	}

	return s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.resolveObjectKey(key)),
	})
}

// resolveObjectKey resolves both relative keys and keys returned by S3 listings.
// The latter already include the store prefix and must not be prefixed twice.
func (s *S3Store) resolveObjectKey(key string) string {
	if s.prefix == "" || key == s.prefix || strings.HasPrefix(key, s.prefix+"/") {
		return key
	}

	return path.Join(s.prefix, key)
}

// PutFileIfMatch publishes one local file only when the expected metadata object is still unchanged.
// A nil identity means the object must not exist.
// This protects the final AES-SIV keyring metadata publication from a racing writer.
func (s *S3Store) PutFileIfMatch(
	ctx context.Context,
	key, filename string,
	expected *ObjectIdentity,
) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}
	if ctx == nil || key == "" || filename == "" {
		return errors.New("S3 object key, file, and context are required")
	}
	if expected != nil && expected.ETag == "" {
		return errors.New("S3 conditional publication requires an expected ETag")
	}

	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open S3 file source: %w", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat S3 file source: %w", err)
	}

	input := &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(path.Join(s.prefix, key)),
		Body:          file,
		ContentLength: aws.Int64(info.Size()),
		ContentType:   aws.String("application/octet-stream"),
	}
	if expected == nil {
		input.IfNoneMatch = aws.String("*")
	} else {
		input.IfMatch = aws.String(expected.ETag)
	}

	if _, err := s.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("conditionally write S3 object %q: %w", key, err)
	}

	return nil
}

// PutFileWithProgress uploads one local archive
// and reports source bytes read by the S3 client.
// The observer is optional and presentation-only.
func (s *S3Store) PutFileWithProgress(
	ctx context.Context,
	key, filename string,
	progress func(int64),
) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}
	if ctx == nil || key == "" || filename == "" {
		return errors.New("S3 object key, file, and context are required")
	}

	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open S3 archive source: %w", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat S3 archive source: %w", err)
	}

	body := io.Reader(file)
	if progress != nil {
		body = &progressReader{reader: file, progress: progress}
	}

	_, err = s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(info.Size()),
		ContentType:   aws.String("application/octet-stream"),
	})
	if err != nil {
		return fmt.Errorf("write S3 archive object %q: %w", key, err)
	}

	return nil
}

// PutReader uploads one stream as an S3 object using multipart upload.
// The object is not visible as a completed object until the reader reaches EOF
// and the multipart upload is committed successfully.
func (s *S3Store) PutReader(ctx context.Context, key string, reader io.Reader, contentType string) error {
	return s.PutReaderWithProgress(ctx, key, reader, contentType, nil)
}

// PutReaderWithProgress uploads one non-seekable stream as a multipart object
// and reports bytes of parts accepted by S3.
// The observer is optional and is called only after each part has been uploaded successfully.
func (s *S3Store) PutReaderWithProgress(
	ctx context.Context,
	key string,
	reader io.Reader,
	contentType string,
	progress func(int64),
) (err error) {
	return s.putReaderWithProgress(ctx, key, reader, contentType, nil, progress)
}

// putReaderWithProgress uploads a stream and optionally attaches object metadata.
func (s *S3Store) putReaderWithProgress(
	ctx context.Context,
	key string,
	reader io.Reader,
	contentType string,
	metadata map[string]string,
	progress func(int64),
) (err error) {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}
	if ctx == nil || key == "" || reader == nil {
		return errors.New("S3 object key, reader, and context are required")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	created, err := s.client.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
		Metadata:    metadata,
	})
	if err != nil {
		return fmt.Errorf("create S3 multipart upload %q: %w", key, err)
	}
	if created.UploadId == nil || *created.UploadId == "" {
		return errors.New("S3 multipart upload returned an empty upload ID")
	}

	uploadID := *created.UploadId
	completed := false
	defer func() {
		// Multipart uploads are invisible as completed objects until commit,
		// but their parts still consume storage. Abort every interrupted upload.
		if completed {
			return
		}

		cleanupContext, cancel := context.WithTimeout(context.Background(), s3CleanupTimeout)
		defer cancel()
		_, abortErr := s.client.AbortMultipartUpload(cleanupContext, &awss3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
		})
		if abortErr != nil {
			err = errors.Join(err, fmt.Errorf("abort S3 multipart upload %q: %w", key, abortErr))
		}
	}()

	buffer := make([]byte, s3MultipartPartSize)
	parts := make([]awss3types.CompletedPart, 0)
	for partNumber := int32(1); ; partNumber++ {
		if partNumber > 10000 {
			return fmt.Errorf("S3 multipart upload %q exceeds the 10000-part limit", key)
		}

		// Read fixed-size parts while allowing the final part
		// to be shorter than the configured multipart size.
		read, readErr := io.ReadFull(reader, buffer)
		if read > 0 {
			output, uploadErr := s.client.UploadPart(ctx, &awss3.UploadPartInput{
				Bucket:        aws.String(s.bucket),
				Key:           aws.String(key),
				UploadId:      aws.String(uploadID),
				PartNumber:    aws.Int32(partNumber),
				Body:          bytes.NewReader(buffer[:read]),
				ContentLength: aws.Int64(int64(read)),
			})
			if uploadErr != nil {
				return fmt.Errorf("upload S3 multipart part %d for %q: %w", partNumber, key, uploadErr)
			}

			parts = append(parts, awss3types.CompletedPart{
				ETag:       output.ETag,
				PartNumber: aws.Int32(partNumber),
			})
			// Advance progress only after S3 accepted the part;
			// failed uploads must not appear to have transferred bytes successfully.
			if progress != nil {
				progress(int64(read))
			}
		}

		switch readErr {
		case nil:
			continue

		case io.ErrUnexpectedEOF:
			if len(parts) == 0 {
				return errors.New("S3 multipart upload received an empty final part")
			}

		case io.EOF:
			if len(parts) == 0 {
				return s.putEmptyWithMetadata(ctx, key, contentType, metadata)
			}

		default:
			return fmt.Errorf("read S3 multipart source: %w", readErr)
		}

		break
	}

	_, err = s.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &awss3types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		return fmt.Errorf("complete S3 multipart upload %q: %w", key, err)
	}

	completed = true
	return nil
}

// putEmptyWithMetadata writes an empty object with optional metadata.
func (s *S3Store) putEmptyWithMetadata(
	ctx context.Context,
	key, contentType string,
	metadata map[string]string,
) error {
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(nil),
		ContentLength: aws.Int64(0),
		ContentType:   aws.String(contentType),
		Metadata:      metadata,
	})
	if err != nil {
		return fmt.Errorf("write empty S3 object %q: %w", key, err)
	}

	return nil
}

// PutBytes uploads one small object under the configured S3 namespace.
// Callers use this for OCI layout metadata;
// large image blobs should use a streaming file or multipart path when that becomes necessary.
func (s *S3Store) PutBytes(ctx context.Context, key string, data []byte, contentType string) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}
	if ctx == nil || key == "" {
		return errors.New("S3 object key and context are required")
	}

	if contentType == "" {
		contentType = "application/octet-stream"
	}

	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(path.Join(s.prefix, key)),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("write S3 object %q: %w", key, err)
	}

	return nil
}

// PutBytesIfMatch publishes one small object with an optimistic concurrency check.
// A nil expected identity means the object must not exist yet.
func (s *S3Store) PutBytesIfMatch(
	ctx context.Context,
	key string,
	data []byte,
	contentType string,
	expected *ObjectIdentity,
) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}
	if ctx == nil || key == "" {
		return errors.New("S3 object key and context are required")
	}
	if expected != nil && expected.ETag == "" {
		return errors.New("S3 conditional publication requires an expected ETag")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	input := &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(path.Join(s.prefix, key)),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(contentType),
	}
	if expected == nil {
		input.IfNoneMatch = aws.String("*")
	} else {
		input.IfMatch = aws.String(expected.ETag)
	}

	if _, err := s.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("conditionally write S3 object %q: %w", key, err)
	}

	return nil
}

// ReadObject downloads one archive object into a local file.
func (s *S3Store) ReadObject(ctx context.Context, key, filename string) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}
	if ctx == nil || key == "" || filename == "" {
		return errors.New("S3 object key, file, and context are required")
	}

	body, err := s.openObject(ctx, key)
	if err != nil {
		return fmt.Errorf("read S3 archive object %q: %w", key, err)
	}
	defer func() { _ = body.Close() }()

	if err := writeFileAtomic(filename, body); err != nil {
		return fmt.Errorf("download S3 archive object %q: %w", key, err)
	}

	return nil
}

// Read writes YAML objects stored below the store prefix in stable key order.
func (s *S3Store) Read(ctx context.Context, output io.Writer) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}
	if ctx == nil || output == nil {
		return errors.New("S3 read context and output are required")
	}

	index := 0
	err := s.forEachResource(ctx, func(_ string, data []byte) error {
		if index > 0 {
			if _, err := io.WriteString(output, "---\n"); err != nil {
				return err
			}
		}

		if _, err := output.Write(data); err != nil {
			return err
		}

		if len(data) == 0 || data[len(data)-1] != '\n' {
			if _, err := io.WriteString(output, "\n"); err != nil {
				return err
			}
		}

		index++
		return nil
	})
	if err != nil {
		return fmt.Errorf("read S3 resources: %w", err)
	}

	return nil
}

// ForEachStoredFile visits every object below the configured prefix in stable key order.
// The callback receives one object body at a time;
// callers should process it before returning to keep memory bounded by one object.
func (s *S3Store) ForEachStoredFile(ctx context.Context, callback func(string, []byte) error) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}

	return s.forEachStoredFile(ctx, callback, nil)
}

// ForEachStoredFileWithProgress visits every stored object
// and reports the number of completed callbacks after the object list has been loaded.
// The observer is optional and cannot affect iteration errors or ordering.
func (s *S3Store) ForEachStoredFileWithProgress(
	ctx context.Context,
	callback func(string, []byte) error,
	observer func(total, completed int),
) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}

	return s.forEachStoredFile(ctx, callback, observer)
}

// ForEachStoredFileStream visits every object below the configured prefix
// in stable key order without buffering its body in memory.
func (s *S3Store) ForEachStoredFileStream(
	ctx context.Context,
	callback func(string, io.Reader) error,
) error {
	return s.forEachStoredFileStream(ctx, callback, nil)
}

// ForEachStoredFileStreamWithProgress is the streaming counterpart
// of ForEachStoredFileWithProgress.
func (s *S3Store) ForEachStoredFileStreamWithProgress(
	ctx context.Context,
	callback func(string, io.Reader) error,
	observer func(total, completed int),
) error {
	return s.forEachStoredFileStream(ctx, callback, observer)
}

// ListStoredObjects lists objects below the configured prefix
// and returns their sizes without issuing GetObject requests.
func (s *S3Store) ListStoredObjects(ctx context.Context) ([]StoredObject, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("S3 store is not initialized")
	}
	if ctx == nil {
		return nil, errors.New("S3 listing context is required")
	}

	objectPrefix := s3DirectoryPrefix(s.prefix)
	pager := awss3.NewListObjectsV2Paginator(s.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(objectPrefix),
	})
	objects := make([]StoredObject, 0)

	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list S3 objects: %w", err)
		}

		for _, item := range page.Contents {
			if item.Key == nil {
				continue
			}

			var size int64
			if item.Size != nil {
				size = *item.Size
			}
			if len(objects) >= maxListedS3Objects {
				return nil, fmt.Errorf("S3 listing contains more than %d objects", maxListedS3Objects)
			}

			objects = append(objects, StoredObject{
				Key:          *item.Key,
				ETag:         aws.ToString(item.ETag),
				Size:         size,
				LastModified: aws.ToTime(item.LastModified),
			})
		}
	}

	slices.SortFunc(objects, func(left, right StoredObject) int {
		return strings.Compare(left.Key, right.Key)
	})

	return objects, nil
}

// s3DirectoryPrefix adds the boundary separator used when listing a logical directory.
func s3DirectoryPrefix(prefix string) string {
	if prefix == "" {
		return ""
	}

	return prefix + "/"
}

// s3RelativeKey returns a key below an S3 directory prefix
// only when the separator boundary is present.
// This excludes sibling prefixes such as backup/resources-old
// when the requested prefix is backup/resources.
func s3RelativeKey(prefix, key string) (string, bool) {
	if prefix == "" {
		return key, key != ""
	}

	boundary := s3DirectoryPrefix(prefix)
	if !strings.HasPrefix(key, boundary) {
		return "", false
	}

	return strings.TrimPrefix(key, boundary), true
}

// DeletePrefix removes all objects below a non-empty relative prefix.
// It is used for cleanup of one failed or expired timestamped capture run.
func (s *S3Store) DeletePrefix(ctx context.Context, prefix string) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}
	if ctx == nil || prefix == "" {
		return errors.New("S3 deletion context and prefix are required")
	}

	fullPrefix := path.Join(s.prefix, prefix)
	pager := awss3.NewListObjectsV2Paginator(s.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fullPrefix + "/"),
	})

	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list S3 objects for deletion: %w", err)
		}

		identifiers := make([]awss3types.ObjectIdentifier, 0, len(page.Contents))
		for _, object := range page.Contents {
			if object.Key != nil {
				identifiers = append(identifiers, awss3types.ObjectIdentifier{Key: object.Key})
			}
		}
		if len(identifiers) == 0 {
			continue
		}

		result, err := s.client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &awss3types.Delete{Objects: identifiers, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete S3 objects below %q: %w", prefix, err)
		}

		if len(result.Errors) != 0 {
			failure := result.Errors[0]
			return fmt.Errorf(
				"delete S3 objects below %q: %s: %s",
				prefix,
				aws.ToString(failure.Code),
				aws.ToString(failure.Message),
			)
		}
	}

	return nil
}

// DeleteObject removes one object below the store prefix.
func (s *S3Store) DeleteObject(ctx context.Context, key string) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}
	if ctx == nil || key == "" {
		return errors.New("S3 deletion context and key are required")
	}

	_, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(path.Join(s.prefix, key)),
	})
	if err != nil {
		return fmt.Errorf("delete S3 object %q: %w", key, err)
	}

	return nil
}

// OpenObject opens one object for streaming consumers
// and transfers response body ownership to the caller.
func (s *S3Store) OpenObject(ctx context.Context, key string) (io.ReadCloser, error) {
	body, _, err := s.OpenObjectWithIdentity(ctx, key, nil)
	return body, err
}

// OpenObjectWithIdentity opens one object and optionally pins the requested identity.
func (s *S3Store) OpenObjectWithIdentity(
	ctx context.Context,
	key string,
	expected *ObjectIdentity,
) (io.ReadCloser, ObjectIdentity, error) {
	if s == nil || s.client == nil {
		return nil, ObjectIdentity{}, errors.New("S3 store is not initialized")
	}
	if ctx == nil || key == "" {
		return nil, ObjectIdentity{}, errors.New("S3 object key and context are required")
	}

	return s.openObjectWithIdentity(ctx, key, expected)
}

// Download copies YAML resources below the S3 prefix into a canonical local directory
// so callers can apply the same rendering pipeline as local sources.
func (s *S3Store) Download(ctx context.Context, root string) error {
	if s == nil || s.client == nil {
		return errors.New("S3 store is not initialized")
	}
	if ctx == nil || root == "" {
		return errors.New("S3 download context and root are required")
	}

	err := s.forEachStoredFileStreamMatching(ctx, func(key string) bool {
		relative, ok := s3RelativeKey(s.prefix, key)
		return ok && (strings.HasSuffix(relative, ".yaml") || strings.HasPrefix(relative, ".kube-dump/crypto/"))
	}, func(key string, data io.Reader) error {
		relative, ok := s3RelativeKey(s.prefix, key)
		if !ok {
			return fmt.Errorf("invalid S3 resource key %q", key)
		}
		if relative == "" || filepath.IsAbs(filepath.FromSlash(relative)) {
			return fmt.Errorf("invalid S3 resource key %q", key)
		}

		target := filepath.Join(root, filepath.FromSlash(relative))
		rel, err := filepath.Rel(root, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("S3 resource key escapes download root: %q", key)
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("create S3 resource directory: %w", err)
		}
		if err := writeFileAtomic(target, data); err != nil {
			return fmt.Errorf("write S3 resource %q: %w", key, err)
		}

		return nil
	}, nil)
	if err != nil {
		return fmt.Errorf("download S3 resources: %w", err)
	}

	return nil
}

// writeFileAtomic writes a downloaded object beside its destination
// and publishes it only after the complete stream has been synced and closed.
// Existing files therefore survive interrupted or malformed downloads.
func writeFileAtomic(filename string, source io.Reader) error {
	if filename == "" || source == nil {
		return errors.New("atomic S3 file path and source are required")
	}

	temporary, err := os.CreateTemp(filepath.Dir(filename), ".kube-dump-s3-*")
	if err != nil {
		return fmt.Errorf("create temporary S3 file: %w", err)
	}

	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set temporary S3 file permissions: %w", err)
	}
	if _, err := io.Copy(temporary, source); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary S3 file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary S3 file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary S3 file: %w", err)
	}
	if err := fileutil.ReplaceFile(temporaryName, filename); err != nil {
		return fmt.Errorf("publish S3 file: %w", err)
	}

	removeTemporary = false
	return nil
}

// forEachResource lists canonical YAML objects in stable key order
// and passes each object body to callback.
// All S3 directory readers share this path so filtering, ordering, pagination,
// and object closing cannot drift apart.
func (s *S3Store) forEachResource(ctx context.Context, callback func(string, []byte) error) error {
	return s.forEachStoredFileMatching(ctx, func(key string) bool {
		return strings.HasSuffix(key, ".yaml")
	}, func(key string, data []byte) error {
		return callback(key, data)
	}, nil)
}

// forEachStoredFileMatching is the byte-buffered counterpart of the filtered streaming iterator.
// It keeps the existing small-object API for callers that need complete YAML or metadata values.
func (s *S3Store) forEachStoredFileMatching(
	ctx context.Context,
	match func(string) bool,
	callback func(string, []byte) error,
	observer func(total, completed int),
) error {
	return s.forEachStoredFileStreamMatching(ctx, match, func(key string, reader io.Reader) error {
		data, err := readObjectLimited(reader, maxBufferedS3ObjectBytes)
		if err != nil {
			return fmt.Errorf("read S3 object %q: %w", key, err)
		}

		return callback(key, data)
	}, observer)
}

// forEachStoredFile lists and reads every object below the configured prefix in stable order.
// Higher-level readers apply their own layout filters.
func (s *S3Store) forEachStoredFile(
	ctx context.Context,
	callback func(string, []byte) error,
	observer func(total, completed int),
) error {
	if ctx == nil || callback == nil {
		return errors.New("S3 store context and callback are required")
	}

	return s.forEachStoredFileStream(ctx, func(key string, reader io.Reader) error {
		data, err := readObjectLimited(reader, maxBufferedS3ObjectBytes)
		if err != nil {
			return fmt.Errorf("read S3 object %q: %w", key, err)
		}

		return callback(key, data)
	}, observer)
}

// readObjectLimited buffers one small S3 object and rejects oversized bodies.
// Callers that handle archives or image blobs must use the streaming iterator.
func readObjectLimited(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, errors.New("S3 object reader is required")
	}
	if limit <= 0 {
		return nil, errors.New("S3 object size limit must be positive")
	}

	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("S3 object exceeds %d bytes", limit)
	}

	return data, nil
}

// forEachStoredFileStream lists objects first and keeps only one response body open at a time,
// so callers can stream large blobs without whole-object buffers.
func (s *S3Store) forEachStoredFileStream(
	ctx context.Context,
	callback func(string, io.Reader) error,
	observer func(total, completed int),
) error {
	return s.forEachStoredFileStreamMatching(ctx, nil, callback, observer)
}

// forEachStoredFileStreamMatching lists objects
// and applies an optional key filter before opening response bodies.
// Object metadata remains bounded by the listing result,
// while irrelevant object bodies are never fetched.
func (s *S3Store) forEachStoredFileStreamMatching(
	ctx context.Context,
	match func(string) bool,
	callback func(string, io.Reader) error,
	observer func(total, completed int),
) error {
	if ctx == nil || callback == nil {
		return errors.New("S3 store context and callback are required")
	}

	objects, err := s.ListStoredObjects(ctx)
	if err != nil {
		return err
	}
	if match != nil {
		objects = slices.DeleteFunc(objects, func(object StoredObject) bool {
			return !match(object.Key)
		})
	}
	if observer != nil {
		observer(len(objects), 0)
	}

	for index, object := range objects {
		body, err := s.openObject(ctx, object.Key)
		if err != nil {
			return fmt.Errorf("read S3 object %q: %w", object.Key, err)
		}

		callbackErr := callback(object.Key, body)
		closeErr := body.Close()
		if callbackErr != nil {
			if closeErr != nil {
				return errors.Join(callbackErr, fmt.Errorf("close S3 object %q: %w", object.Key, closeErr))
			}
			return callbackErr
		}
		if closeErr != nil {
			return fmt.Errorf("close S3 object %q: %w", object.Key, closeErr)
		}
		if observer != nil {
			observer(len(objects), index+1)
		}
	}

	return nil
}

// openObject opens one S3 object and leaves response-body ownership with the caller.
// Streaming callers use it to avoid buffering large archive objects.
func (s *S3Store) openObject(ctx context.Context, key string) (io.ReadCloser, error) {
	body, _, err := s.openObjectWithIdentity(ctx, key, nil)
	return body, err
}

// openObjectWithIdentity performs the GET used by streaming consumers.
func (s *S3Store) openObjectWithIdentity(
	ctx context.Context,
	key string,
	expected *ObjectIdentity,
) (io.ReadCloser, ObjectIdentity, error) {
	input := &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.resolveObjectKey(key)),
	}
	if expected != nil {
		if expected.ETag != "" {
			input.IfMatch = aws.String(expected.ETag)
		}
		if expected.VersionID != "" {
			input.VersionId = aws.String(expected.VersionID)
		}
	}

	object, err := s.client.GetObject(ctx, input)
	if err != nil {
		return nil, ObjectIdentity{}, err
	}

	identity := ObjectIdentity{
		ETag:      aws.ToString(object.ETag),
		VersionID: aws.ToString(object.VersionId),
		Size:      aws.ToInt64(object.ContentLength),
	}
	if err := identity.Validate(); err != nil {
		_ = object.Body.Close()
		return nil, ObjectIdentity{}, fmt.Errorf("cannot pin S3 object %q: %w", key, err)
	}
	if expected != nil && identity != *expected {
		_ = object.Body.Close()
		return nil, ObjectIdentity{}, fmt.Errorf("S3 object %q changed between reads", key)
	}

	return object.Body, identity, nil
}

// NewS3Store creates an S3 resource store using explicit credentials when set,
// otherwise allowing the AWS SDK default credential chain to resolve them.
func NewS3Store(ctx context.Context, options S3Options) (*S3Store, error) {
	if ctx == nil {
		return nil, errors.New("S3 context is required")
	}
	if err := ValidateEndpoint(options.Endpoint, options.Insecure); err != nil {
		return nil, err
	}
	if options.Insecure && strings.TrimSpace(options.Endpoint) != "" {
		log.Warn().
			Str("component", "s3").
			Str("endpoint", options.Endpoint).
			Msg("S3 endpoint security checks are relaxed")
	}

	bucket, prefix, err := resolveLocation(options)
	if err != nil {
		return nil, err
	}

	loadOptions := make([]func(*awsconfig.LoadOptions) error, 0, 2)
	loadOptions = append(loadOptions, awsconfig.WithRetryer(func() aws.Retryer {
		return awsretry.NewStandard(func(options *awsretry.StandardOptions) {
			options.MaxAttempts = retry.Attempts(ctx)
		})
	}))
	if options.Region != "" {
		loadOptions = append(loadOptions, awsconfig.WithRegion(options.Region))
	}

	if options.AccessKey != "" || options.SecretKey != "" {
		if options.AccessKey == "" || options.SecretKey == "" {
			return nil, errors.New("S3 access key and secret key must be provided together")
		}

		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(options.AccessKey, options.SecretKey, ""),
		))
	}

	config, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load S3 configuration: %w", err)
	}

	clientOptions := func(clientOptions *awss3.Options) {
		clientOptions.UsePathStyle = true
		if options.Endpoint != "" {
			clientOptions.BaseEndpoint = aws.String(options.Endpoint)
		}
	}

	return &S3Store{
		client: awss3.NewFromConfig(config, clientOptions),
		bucket: bucket,
		prefix: prefix,
	}, nil
}

// Write serializes one object and replaces its S3 object only when its digest changes.
func (s *S3Store) Write(ctx context.Context, object state.Object) (bool, error) {
	if s == nil || s.client == nil {
		return false, errors.New("S3 store is not initialized")
	}
	if ctx == nil {
		return false, errors.New("S3 write context is required")
	}

	data, err := codec.Marshal(object)
	if err != nil {
		return false, err
	}

	digest := sha256.Sum256(data)
	digestValue := hex.EncodeToString(digest[:])
	key, err := s.objectKey(object.Identity)
	if err != nil {
		return false, err
	}

	head, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil && head.Metadata[contentDigestMetadata] == digestValue {
		return false, nil
	}
	if err != nil && !isS3NotFound(err) {
		return false, fmt.Errorf("inspect S3 object %q: %w", key, err)
	}

	_, err = s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/yaml"),
		Metadata:    map[string]string{contentDigestMetadata: digestValue},
	})
	if err != nil {
		return false, fmt.Errorf("write S3 object %q: %w", key, err)
	}

	return true, nil
}

// objectKey maps an object identity to a stable key below the S3 prefix.
func (s *S3Store) objectKey(identity state.Identity) (string, error) {
	relative, err := CanonicalObjectPath(identity)
	if err != nil {
		return "", err
	}

	return path.Join(s.prefix, relative), nil
}

// resolveLocation combines URI and explicit bucket/prefix overrides.
func resolveLocation(options S3Options) (string, string, error) {
	bucket, prefix := options.Bucket, strings.Trim(options.Prefix, "/")
	if options.URI != "" {
		location, err := url.Parse(options.URI)
		if err != nil || location.Scheme != "s3" || location.Host == "" {
			return "", "", fmt.Errorf("invalid S3 URI %q", options.URI)
		}

		bucket = location.Host
		prefix = strings.Trim(path.Join(location.Path, options.Prefix), "/")
	}

	if bucket == "" {
		return "", "", errors.New("S3 bucket is required")
	}

	return bucket, prefix, nil
}

// isS3NotFound reports whether an SDK error represents a missing object.
func isS3NotFound(err error) bool {
	if err == nil {
		return false
	}

	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}

	var responseErr *smithyhttp.ResponseError
	return errors.As(err, &responseErr) && responseErr.HTTPStatusCode() == 404
}
