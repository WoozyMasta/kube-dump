// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	"github.com/woozymasta/kube-dump/v2/internal/retry"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const defaultS3MoverCompletionWait = 24 * time.Hour

var secretResource = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// S3MoverOptions configures a helper Pod that streams one PVC archive directly into an S3-compatible object store.
type S3MoverOptions struct {
	// Dynamic manages the temporary Secret and mover Pod lifecycle.
	Dynamic dynamic.Interface
	// TransferLimiter bounds concurrent mover Pod data streams.
	// A nil value uses DefaultTransferConcurrency.
	TransferLimiter *TransferLimiter
	// Image is the exact image containing the volume upload command.
	Image string
	// ImagePullPolicy controls when Kubernetes pulls the mover image.
	ImagePullPolicy corev1.PullPolicy
	// AgentBinary is the executable path inside the mover image.
	AgentBinary string
	// URI is the base S3 prefix under which each PVC gets its own timestamped artifact directory.
	URI string
	// Endpoint overrides the S3-compatible API endpoint.
	Endpoint string
	// Region selects the S3 region.
	Region string
	// Compression identifies the archive codec.
	Compression compress.Algorithm
	// AccessKey is an optional static S3 access key.
	AccessKey string
	// SecretKey is an optional static S3 secret key.
	SecretKey string
	// RunID identifies temporary resources created by this invocation.
	RunID string
	// Recipients contains validated age recipient text passed to the mover.
	Recipients []string
	// RetryAttempts is the maximum number of attempts for retryable requests made by the helper.
	RetryAttempts int
	// CompletionTimeout bounds one mover Pod operation.
	CompletionTimeout time.Duration
	// Insecure permits plaintext HTTP and loopback development endpoints.
	Insecure bool
}

// S3Mover streams one PVC archive from a mounted helper Pod to S3.
// It implements BackupStrategy so SnapshotCopyStrategy
// can reuse the same uploader after it has created a temporary snapshot clone.
type S3Mover struct {
	// options contains the validated mover configuration.
	options S3MoverOptions
}

// moverResult is the JSON contract emitted by the hidden volume upload command.
type moverResult struct {
	Metadata *Metadata `json:"metadata,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// NewS3Mover validates the direct-to-S3 helper contract.
func NewS3Mover(options S3MoverOptions) (*S3Mover, error) {
	if options.Dynamic == nil {
		return nil, errors.New("S3 mover dynamic client is required")
	}
	if options.Image == "" {
		return nil, errors.New("S3 mover container image is required")
	}
	if options.URI == "" {
		return nil, errors.New("S3 mover URI is required")
	}
	if options.AccessKey != "" || options.SecretKey != "" {
		if options.AccessKey == "" || options.SecretKey == "" {
			return nil, errors.New("S3 mover access key and secret key must be provided together")
		}
	}

	if options.Compression == "" {
		options.Compression = compress.DefaultAlgorithm
	}
	if err := (compress.Config{Algorithm: options.Compression}).Validate(); err != nil {
		return nil, fmt.Errorf("validate S3 mover compression: %w", err)
	}

	if options.CompletionTimeout == 0 {
		options.CompletionTimeout = defaultS3MoverCompletionWait
	}
	if options.CompletionTimeout < 0 {
		return nil, errors.New("S3 mover completion timeout must not be negative")
	}

	if options.TransferLimiter == nil {
		options.TransferLimiter = NewTransferLimiter(DefaultTransferConcurrency)
	}

	return &S3Mover{options: options}, nil
}

// Backup creates one short-lived mover Pod and waits until its S3 upload is complete.
// The destination writer is unused because the data plane runs in the cluster.
// The common strategy contract keeps snapshot-copy composition identical for local and direct-S3 destinations.
func (m *S3Mover) Backup(ctx context.Context, claim Ref, _ io.Writer) (metadata Metadata, err error) {
	if m == nil {
		return Metadata{}, errors.New("S3 mover is nil")
	}
	if ctx == nil {
		return Metadata{}, errors.New("S3 mover context is required")
	}
	if err := claim.Validate(); err != nil {
		return Metadata{}, err
	}
	defer func() {
		if err == nil || IsCleanupOnly(err) {
			reportProgress(ctx, ProgressEvent{Stage: StageCompleted, Percent: 100})
		}
	}()

	// The transfer limiter protects the cluster and the S3 endpoint
	// from an unbounded number of concurrent mover Pods.
	reportProgress(ctx, ProgressEvent{Stage: StagePreparing, Percent: 5})
	if err := m.options.TransferLimiter.Acquire(ctx); err != nil {
		return Metadata{}, fmt.Errorf("wait for S3 transfer slot: %w", err)
	}
	defer m.options.TransferLimiter.Release()

	timestamp, err := NewRevision()
	if err != nil {
		return Metadata{}, err
	}

	secretName, err := m.createCredentialsSecret(ctx, claim)
	if err != nil {
		return Metadata{}, err
	}
	secretCleanup := func() error {
		if secretName == "" {
			return nil
		}

		cleanupContext, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := m.options.Dynamic.Resource(secretResource).
			Namespace(claim.Namespace).
			Delete(cleanupContext, secretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete S3 credentials Secret %q: %w", secretName, err)
		}

		return nil
	}
	defer func() {
		if cleanupErr := secretCleanup(); cleanupErr != nil {
			err = errors.Join(err, &CleanupWarning{
				Resource: fmt.Sprintf("S3 credentials Secret %s/%s", claim.Namespace, secretName),
				Err:      cleanupErr,
			})
		}
	}()

	helperOptions := m.helperOptions(claim, secretName, timestamp)
	agentBinaries := helperAgentBinaries(m.options.AgentBinary)
	var resultData string
	var cleanupWarning error
	for index, agentBinary := range agentBinaries {
		// Retry only an agent startup failure with the next configured binary;
		// data-plane errors must be returned instead of being hidden by a fallback.
		helperOptions.AgentBinary = agentBinary
		pod, startErr := helperOptions.Start(ctx, claim, "kube-dump-mover-")
		if startErr != nil {
			return Metadata{}, startErr
		}

		reportProgress(ctx, ProgressEvent{Stage: StageHelperReady, Percent: 20})
		reportProgress(ctx, ProgressEvent{Stage: StageUploading, Percent: 25})

		var operationErr error
		resultData, operationErr = helperOptions.WaitCompleted(ctx, pod, m.options.CompletionTimeout)
		cleanupContext, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		cleanupErr := helperOptions.Cleanup(cleanupContext, pod)
		cancel()

		if cleanupErr != nil {
			cleanupWarning = errors.Join(cleanupWarning, &CleanupWarning{
				Resource: fmt.Sprintf("mover Pod %s/%s", pod.Namespace, pod.Name),
				Err:      cleanupErr,
			})
		}

		if operationErr == nil {
			break
		}
		if cleanupWarning != nil || index+1 == len(agentBinaries) ||
			!errors.Is(operationErr, errHelperAgentStart) || ctx.Err() != nil {
			return Metadata{}, errors.Join(operationErr, cleanupWarning)
		}
	}

	// The helper returns a bounded JSON result through its termination message;
	// validate it before exposing metadata to the common PVC store.
	decodedMetadata, decodeErr := decodeMoverResult(resultData)
	if decodeErr != nil {
		return Metadata{}, decodeErr
	}

	reportProgress(ctx, ProgressEvent{Stage: StageFinalizing, Percent: 90})

	return decodedMetadata, cleanupWarning
}

// decodeMoverResult accepts error results without requiring an invalid zero metadata value.
func decodeMoverResult(data string) (Metadata, error) {
	var result moverResult
	if err := json.Unmarshal([]byte(data), &result); err != nil {
		return Metadata{}, fmt.Errorf("decode mover Pod result: %w", err)
	}
	if result.Error != "" {
		return Metadata{}, errors.New(result.Error)
	}
	if result.Metadata == nil {
		return Metadata{}, errors.New("mover Pod result does not contain metadata")
	}
	if err := result.Metadata.Validate(); err != nil {
		return Metadata{}, fmt.Errorf("validate mover Pod metadata: %w", err)
	}

	return *result.Metadata, nil
}

// createCredentialsSecret creates credentials only when static credentials were explicitly supplied.
// An empty pair leaves credential resolution to the AWS SDK inside the Pod,
// which supports ambient cluster identity providers.
func (m *S3Mover) createCredentialsSecret(ctx context.Context, claim Ref) (string, error) {
	if m.options.AccessKey == "" && m.options.SecretKey == "" {
		return "", nil
	}

	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"generateName": "kube-dump-s3-",
			"labels":       temporaryResourceLabels(claim, m.options.RunID),
		},
		"type": "Opaque",
		"stringData": map[string]any{
			"AWS_ACCESS_KEY_ID":     m.options.AccessKey,
			"AWS_SECRET_ACCESS_KEY": m.options.SecretKey,
		},
	}}

	created, err := m.options.Dynamic.Resource(secretResource).
		Namespace(claim.Namespace).
		Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create S3 credentials Secret: %w", err)
	}

	return created.GetName(), nil
}

// helperOptions builds a one-shot Pod command with no shell interpolation.
func (m *S3Mover) helperOptions(claim Ref, secretName, timestamp string) HelperPodOptions {
	object := path.Join(
		"volumes",
		state.EncodePathSegment(claim.Namespace),
		state.EncodePathSegment(claim.Name),
		timestamp,
	)

	arguments := []string{
		"volume",
		"upload",
		m.options.URI,
		object,
		"/data",
		"/dev/termination-log",
		"--strategy=pod",
		"--compression=" + string(m.options.Compression),
		"--retry-attempts=" + strconv.Itoa(retry.NormalizeAttempts(m.options.RetryAttempts)),
	}

	if m.options.Endpoint != "" {
		arguments = append(arguments, "--s3-endpoint="+m.options.Endpoint)
	}
	if m.options.Insecure {
		arguments = append(arguments, "--s3-insecure")
	}
	if m.options.Region != "" {
		arguments = append(arguments, "--s3-region="+m.options.Region)
	}
	for _, recipient := range m.options.Recipients {
		if strings.TrimSpace(recipient) == "" {
			continue
		}

		arguments = append(arguments, "--recipient="+recipient)
	}

	secrets := []string(nil)
	if secretName != "" {
		secrets = []string{secretName}
	}

	return HelperPodOptions{
		Dynamic:                      m.options.Dynamic,
		Image:                        m.options.Image,
		ImagePullPolicy:              m.options.ImagePullPolicy,
		AgentBinary:                  m.options.AgentBinary,
		MountPath:                    "/data",
		ReadOnly:                     true,
		RunID:                        m.options.RunID,
		AutomountServiceAccountToken: secretName == "",
		Arguments:                    arguments,
		EnvironmentFromSecrets:       secrets,
	}
}
