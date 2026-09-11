// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/woozymasta/kube-dump/v2/internal/compress"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const cleanupTimeout = 30 * time.Second

var pvcResource = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}

// PodStrategyOptions configures direct PVC streaming through a helper Pod.
type PodStrategyOptions struct {
	// Dynamic is the Kubernetes dynamic client used for PVC and Pod lifecycle.
	Dynamic dynamic.Interface
	// Exec streams the internal volume-agent command in the helper Pod.
	Exec ExecTransport
	// TransferLimiter bounds active data streams shared by PVC tasks.
	// A nil value uses DefaultTransferConcurrency.
	TransferLimiter *TransferLimiter
	// Image is the exact container image reference used by temporary Pods.
	Image string
	// Compression selects the artifact stream compression algorithm.
	Compression compress.Algorithm
	// Recipients enables binary age encryption of the volume stream.
	Recipients []age.Recipient
	// Helper contains helper Pod defaults and readiness settings.
	Helper HelperPodOptions
}

// PodStrategy implements the portable direct helper-Pod PVC strategy.
type PodStrategy struct {
	// options contains the validated clients and strategy behavior.
	options PodStrategyOptions
}

// countWriter counts uncompressed bytes while forwarding them to a writer.
type countWriter struct {
	// writer receives the forwarded stream bytes.
	writer io.Writer
	// bytes is the number of bytes accepted by writer.
	bytes int64
}

// limitedBuffer bounds diagnostic output from a helper command.
type limitedBuffer struct {
	// buffer stores the retained diagnostic prefix.
	buffer *bytes.Buffer
	// limit bounds the number of retained bytes.
	limit int
}

// NewPodStrategy validates options and creates a direct PVC strategy.
func NewPodStrategy(options PodStrategyOptions) (*PodStrategy, error) {
	if options.Dynamic == nil {
		return nil, errors.New("PVC Pod strategy dynamic client is required")
	}
	if options.Exec == nil {
		return nil, errors.New("PVC Pod strategy exec transport is required")
	}
	if options.Image == "" {
		return nil, errors.New("PVC Pod strategy container image is required")
	}

	if options.Compression == "" {
		options.Compression = compress.DefaultAlgorithm
	}
	if err := (compress.Config{Algorithm: options.Compression}).Validate(); err != nil {
		return nil, fmt.Errorf("validate PVC strategy compression: %w", err)
	}

	options.Helper.Dynamic = options.Dynamic
	options.Helper.Image = options.Image
	if options.Helper.MountPath == "" {
		options.Helper.MountPath = "/data"
	}
	if options.TransferLimiter == nil {
		options.TransferLimiter = NewTransferLimiter(DefaultTransferConcurrency)
	}

	return &PodStrategy{options: options}, nil
}

// Backup streams the helper Pod filesystem export through the selected codec.
//
// The helper is created for one operation and always cleaned up before the method returns.
// Metadata describes the uncompressed stream,
// so restore can validate the payload without depending on the transport codec.
func (s *PodStrategy) Backup(ctx context.Context, pvc Ref, dst io.Writer) (metadata Metadata, err error) {
	if err := s.validateStreamCall(ctx, pvc, dst); err != nil {
		return Metadata{}, err
	}
	defer func() {
		if err == nil || IsCleanupOnly(err) {
			reportProgress(ctx, ProgressEvent{Stage: StageCompleted, Percent: 100})
		}
	}()

	reportProgress(ctx, ProgressEvent{Stage: StagePreparing, Percent: 5})
	if err := s.options.TransferLimiter.Acquire(ctx); err != nil {
		return Metadata{}, fmt.Errorf("wait for PVC transfer slot: %w", err)
	}
	defer s.options.TransferLimiter.Release()

	// The helper Pod is the data-plane boundary:
	// Kubernetes provisions and mounts it, while the agent emits the portable archive stream below.
	pod, err := s.startHelper(ctx, pvc, true)
	if err != nil {
		return Metadata{}, err
	}
	reportProgress(ctx, ProgressEvent{Stage: StageHelperReady, Percent: 20})
	defer func() {
		if cleanupErr := s.cleanupHelper(ctx, pod); cleanupErr != nil {
			err = errors.Join(err, &CleanupWarning{
				Resource: fmt.Sprintf("helper Pod %s/%s", pod.Namespace, pod.Name),
				Err:      cleanupErr,
			})
		}
	}()

	// Compose the stream as agent -> compressor -> optional age encryptor -> dst.
	// Closing happens in reverse order so every layer flushes before metadata is finalized.
	streamDestination := dst
	var encryptor io.WriteCloser
	if len(s.options.Recipients) > 0 {
		encryptor, err = agecrypto.NewEncryptWriter(dst, s.options.Recipients, agecrypto.EncryptOptions{})
		if err != nil {
			return Metadata{}, fmt.Errorf("create PVC stream encryptor: %w", err)
		}
		streamDestination = encryptor
	}

	compressor, err := compress.NewWriter(streamDestination, compress.Config{Algorithm: s.options.Compression})
	if err != nil {
		if encryptor != nil {
			_ = encryptor.Close()
		}
		return Metadata{}, fmt.Errorf("create PVC stream compressor: %w", err)
	}

	hash := sha256.New()
	count := &countWriter{writer: io.MultiWriter(compressor, hash)}
	reportProgress(ctx, ProgressEvent{Stage: StageStreaming, Percent: 25})
	if err := s.exec(ctx, pod, []string{pod.AgentBinary, "volume", "export", pod.MountPath}, nil, count); err != nil {
		_ = compressor.Close()
		return Metadata{}, err
	}
	if err := compressor.Close(); err != nil {
		return Metadata{}, fmt.Errorf("close PVC stream compressor: %w", err)
	}
	if encryptor != nil {
		if err := encryptor.Close(); err != nil {
			return Metadata{}, fmt.Errorf("close PVC stream encryptor: %w", err)
		}
	}

	reportProgress(ctx, ProgressEvent{Stage: StageFinalizing, Percent: 90})

	return Metadata{
		Version:       metadataVersion,
		Strategy:      Pod,
		Compression:   string(s.options.Compression),
		Encrypted:     len(s.options.Recipients) > 0,
		Portable:      true,
		SizeBytes:     count.bytes,
		ContentSHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

// Restore streams a portable archive into the target PVC through a helper Pod.
// The target PVC must already exist; this method never creates or deletes it.
func (s *PodStrategy) Restore(ctx context.Context, target Ref, src io.Reader, policy ExistingPolicy) (err error) {
	if s == nil {
		return errors.New("PVC Pod strategy is nil")
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if ctx == nil || src == nil {
		return errors.New("PVC restore context and source are required")
	}
	if err := policy.Validate(); err != nil {
		return err
	}

	reportProgress(ctx, ProgressEvent{Stage: StagePreparing, Percent: 5})
	pod, err := s.startHelper(ctx, target, false)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := s.cleanupHelper(ctx, pod); cleanupErr != nil {
			err = errors.Join(err, &CleanupWarning{
				Resource: fmt.Sprintf("helper Pod %s/%s", pod.Namespace, pod.Name),
				Err:      cleanupErr,
			})
		}
	}()

	reportProgress(ctx, ProgressEvent{Stage: StageHelperReady, Percent: 20})
	reportProgress(ctx, ProgressEvent{Stage: StageStreaming, Percent: 25})
	if err := s.exec(ctx, pod, []string{
		pod.AgentBinary,
		"volume",
		"import",
		pod.MountPath,
		string(policy),
	}, src, io.Discard); err != nil {
		return err
	}

	reportProgress(ctx, ProgressEvent{Stage: StageFinalizing, Percent: 90})
	reportProgress(ctx, ProgressEvent{Stage: StageCompleted, Percent: 100})

	return nil
}

// validateStreamCall checks common backup preconditions.
func (s *PodStrategy) validateStreamCall(ctx context.Context, pvc Ref, dst io.Writer) error {
	if s == nil {
		return errors.New("PVC Pod strategy is nil")
	}
	if err := pvc.Validate(); err != nil {
		return err
	}
	if ctx == nil || dst == nil {
		return errors.New("PVC backup context and destination are required")
	}

	return nil
}

// startHelper creates a temporary read-only helper Pod
// and selects a node compatible with the source PVC.
func (s *PodStrategy) startHelper(ctx context.Context, pvc Ref, readOnly bool) (HelperPod, error) {
	options, err := s.prepareHelperOptions(ctx, pvc, readOnly, nil)
	if err != nil {
		return HelperPod{}, fmt.Errorf("prepare helper Pod: %w", err)
	}

	agentBinaries := helperAgentBinaries(s.options.Helper.AgentBinary)
	for index, agentBinary := range agentBinaries {
		options.AgentBinary = agentBinary
		pod, err := options.Start(ctx, pvc, helperPodName(pvc))
		if err != nil {
			return HelperPod{}, err
		}

		if err := options.WaitReady(ctx, pod); err != nil {
			if cleanupErr := s.cleanupHelper(ctx, pod); cleanupErr != nil {
				return HelperPod{}, errors.Join(err, cleanupErr)
			}
			if index+1 == len(agentBinaries) || !errors.Is(err, errHelperAgentStart) || ctx.Err() != nil {
				return HelperPod{}, err
			}

			continue
		}

		return pod, nil
	}

	return HelperPod{}, errors.New("helper Pod agent did not start")
}

// prepareHelperOptions resolves PVC mode and current attachment state
// before either a dry-run or a real helper Pod request is made.
func (s *PodStrategy) prepareHelperOptions(
	ctx context.Context,
	pvc Ref,
	readOnly bool,
	claim *unstructured.Unstructured,
) (HelperPodOptions, error) {
	inspectUsage := claim == nil
	if inspectUsage {
		var err error
		claim, err = s.options.Dynamic.Resource(pvcResource).
			Namespace(pvc.Namespace).
			Get(ctx, pvc.Name, metav1.GetOptions{})
		if err != nil {
			return HelperPodOptions{}, fmt.Errorf("get PVC %q: %w", pvc.Name, err)
		}
	}

	volumeMode, _, err := unstructured.NestedString(claim.Object, "spec", "volumeMode")
	if err != nil {
		return HelperPodOptions{}, fmt.Errorf("read PVC %q volume mode: %w", pvc.Name, err)
	}
	if volumeMode == "Block" {
		return HelperPodOptions{}, errors.New("direct Pod strategy does not support block-mode PVCs")
	}

	options := s.options.Helper
	options.Dynamic = s.options.Dynamic
	options.Image = s.options.Image
	options.ReadOnly = readOnly
	if err := options.normalize(ctx, pvc, helperPodName(pvc)); err != nil {
		return HelperPodOptions{}, err
	}

	if !inspectUsage {
		return options, nil
	}

	usage, err := InspectUsage(ctx, s.options.Dynamic, pvc)
	if err != nil {
		return HelperPodOptions{}, err
	}

	options.NodeName, err = SelectHelperNode(usage)
	if err != nil {
		return HelperPodOptions{}, err
	}

	return options, nil
}

// cleanupHelper removes a helper Pod while preserving the original operation error.
func (s *PodStrategy) cleanupHelper(_ context.Context, pod HelperPod) error {
	// The operation context is often cancelled precisely because the helper timed out.
	// Cleanup gets a short independent budget
	// so cancellation does not leak the temporary Pod and its volume attachment.
	cleanupContext, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := s.options.Helper.Cleanup(cleanupContext, pod); err != nil {
		return fmt.Errorf("cleanup PVC helper Pod %q: %w", pod.Name, err)
	}

	return nil
}

// exec invokes the volume command with the helper's actual mount path.
// Arguments are passed directly to Kubernetes exec
// so paths remain intact even when a future validated mount-path policy
// permits spaces or other separators.
func (s *PodStrategy) exec(
	ctx context.Context,
	pod HelperPod,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
) error {
	if len(args) == 0 || args[0] != pod.AgentBinary {
		return errors.New("volume agent command is required")
	}

	request := ExecRequest{
		Namespace: pod.Namespace,
		Pod:       pod.Name,
		Container: pod.Container,
		Command:   args,
	}
	var stderr bytes.Buffer
	if err := s.options.Exec.Exec(
		ctx, request, stdin, stdout,
		&limitedBuffer{buffer: &stderr, limit: 64 << 10},
	); err != nil {
		if stderr.Len() > 0 {
			return fmt.Errorf("execute volume agent: %w: %s", err, strings.TrimSpace(stderr.String()))
		}

		return fmt.Errorf("execute volume agent: %w", err)
	}

	return nil
}

// helperPodName returns a DNS-compatible prefix for a temporary PVC helper.
func helperPodName(_ Ref) string {
	return "kube-dump-"
}

// Write forwards bytes and records the number successfully accepted.
func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.bytes += int64(n)

	return n, err
}

// Write retains only a bounded diagnostic prefix and reports all bytes accepted.
func (w *limitedBuffer) Write(p []byte) (int, error) {
	remaining := w.limit - w.buffer.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = w.buffer.Write(p[:remaining])
	}

	return len(p), nil
}
