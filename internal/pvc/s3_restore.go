// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/woozymasta/kube-dump/v2/internal/compress"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

const defaultS3RestorerCompletionWait = 24 * time.Hour

// S3RestorerOptions configures a helper Pod that downloads one PVC archive directly from S3.
type S3RestorerOptions struct {
	// Dynamic manages temporary Secrets and the helper Pod lifecycle.
	Dynamic dynamic.Interface
	// TransferLimiter bounds concurrent restore data streams.
	TransferLimiter *TransferLimiter
	// ExpectedTargetIdentity rejects a changed or recreated restore target.
	ExpectedTargetIdentity *TargetIdentity
	// Image is the exact image containing the volume download command.
	Image string
	// ImagePullPolicy controls when Kubernetes pulls the restore image.
	ImagePullPolicy corev1.PullPolicy
	// AgentBinary is the executable path inside the restore image.
	AgentBinary string
	// URI is the S3 bucket and prefix used to resolve the object key.
	URI string
	// Endpoint overrides the S3-compatible API endpoint.
	Endpoint string
	// Region selects the S3 region.
	Region string
	// Object is the complete S3 object key of the compressed archive.
	Object string
	// Compression identifies the archive codec.
	Compression compress.Algorithm
	// Existing controls how the target PVC's existing data is handled.
	Existing ExistingPolicy
	// ExpectedSHA256 is the uncompressed tar payload digest from metadata.
	ExpectedSHA256 string
	// IdentityPassphrase unlocks an encrypted SSH identity inside the helper.
	IdentityPassphrase string
	// AccessKey are optional static S3 credentials.
	AccessKey string
	// SecretKey are optional static S3 credentials.
	SecretKey string
	// RunID identifies temporary resources created by this invocation.
	RunID string
	// Identity contains the private identity file content for encrypted archives.
	Identity []byte
	// ExpectedSize is the uncompressed tar payload size from metadata.
	ExpectedSize int64
	// ExpectedTargetSize is the minimum restore payload size accepted by the target PVC.
	ExpectedTargetSize int64
	// CompletionTimeout bounds one helper Pod operation.
	CompletionTimeout time.Duration
	// Insecure permits plaintext HTTP and loopback development endpoints.
	Insecure bool
	// Encrypted tells the helper to decrypt the object with the mounted identity.
	Encrypted bool
}

// S3Restorer downloads one archive inside a helper Pod and imports it into a mounted PVC.
type S3Restorer struct {
	// options contains the validated S3, helper, and archive settings.
	options S3RestorerOptions
}

// NewS3Restorer validates the direct S3 restore contract.
func NewS3Restorer(options S3RestorerOptions) (*S3Restorer, error) {
	if options.Dynamic == nil {
		return nil, errors.New("S3 restorer dynamic client is required")
	}
	if options.Image == "" {
		return nil, errors.New("S3 restorer container image is required")
	}
	if options.URI == "" {
		return nil, errors.New("S3 restorer URI is required")
	}
	if options.Object == "" {
		return nil, errors.New("S3 restorer object is required")
	}
	if options.AccessKey != "" || options.SecretKey != "" {
		if options.AccessKey == "" || options.SecretKey == "" {
			return nil, errors.New("S3 restorer access key and secret key must be provided together")
		}
	}

	if options.Compression == "" {
		options.Compression = compress.DefaultAlgorithm
	}

	if err := (compress.Config{Algorithm: options.Compression}).Validate(); err != nil {
		return nil, fmt.Errorf("validate S3 restorer compression: %w", err)
	}
	if err := options.Existing.Validate(); err != nil {
		return nil, err
	}
	if options.ExpectedSize < 0 {
		return nil, errors.New("S3 restorer expected size must not be negative")
	}
	if options.Encrypted && len(options.Identity) == 0 {
		return nil, errors.New("S3 restorer identity is required for encrypted archives")
	}

	if options.CompletionTimeout == 0 {
		options.CompletionTimeout = defaultS3RestorerCompletionWait
	}

	if options.CompletionTimeout < 0 {
		return nil, errors.New("S3 restorer completion timeout must not be negative")
	}
	if options.TransferLimiter == nil {
		options.TransferLimiter = NewTransferLimiter(DefaultTransferConcurrency)
	}

	return &S3Restorer{options: options}, nil
}

// Restore creates one helper Pod and waits until it has downloaded and imported the archive.
func (r *S3Restorer) Restore(ctx context.Context, target Ref) (err error) {
	if r == nil {
		return errors.New("S3 restorer is nil")
	}
	if ctx == nil {
		return errors.New("S3 restorer context is required")
	}
	if err := target.Validate(); err != nil {
		return err
	}

	defer func() {
		if err == nil || IsCleanupOnly(err) {
			reportProgress(ctx, ProgressEvent{Stage: StageCompleted, Percent: 100})
		}
	}()
	reportProgress(ctx, ProgressEvent{Stage: StagePreparing, Percent: 5})

	if err := r.options.TransferLimiter.Acquire(ctx); err != nil {
		return fmt.Errorf("wait for S3 restore transfer slot: %w", err)
	}
	defer r.options.TransferLimiter.Release()

	// Static credentials and private identities are short-lived resources;
	// create each only when the helper actually needs it and clean it up on return.
	credentialSecret, err := r.createSecret(ctx, target, "credentials", map[string]string{
		"AWS_ACCESS_KEY_ID":     r.options.AccessKey,
		"AWS_SECRET_ACCESS_KEY": r.options.SecretKey,
	})
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := r.deleteSecret(target.Namespace, credentialSecret); cleanupErr != nil {
			err = errors.Join(err, &CleanupWarning{
				Resource: fmt.Sprintf("S3 restore credentials Secret %s/%s", target.Namespace, credentialSecret),
				Err:      cleanupErr,
			})
		}
	}()

	identitySecret, err := r.createSecret(ctx, target, "identity", map[string]string{
		"identity": string(r.options.Identity),
	})
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := r.deleteSecret(target.Namespace, identitySecret); cleanupErr != nil {
			err = errors.Join(err, &CleanupWarning{
				Resource: fmt.Sprintf("S3 restore identity Secret %s/%s", target.Namespace, identitySecret),
				Err:      cleanupErr,
			})
		}
	}()

	passphraseSecret, err := r.createSecret(ctx, target, "passphrase", map[string]string{
		"KUBE_DUMP_IDENTITY_PASSPHRASE": r.options.IdentityPassphrase,
	})
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := r.deleteSecret(target.Namespace, passphraseSecret); cleanupErr != nil {
			err = errors.Join(err, &CleanupWarning{
				Resource: fmt.Sprintf("S3 restore passphrase Secret %s/%s", target.Namespace, passphraseSecret),
				Err:      cleanupErr,
			})
		}
	}()

	helperOptions := r.helperOptions(credentialSecret, identitySecret, passphraseSecret)
	agentBinaries := helperAgentBinaries(r.options.AgentBinary)

	for index, agentBinary := range agentBinaries {
		// Retry only when the configured executable cannot start;
		// data-plane failures must not be hidden by trying another binary.
		helperOptions.AgentBinary = agentBinary
		pod, startErr := helperOptions.Start(ctx, target, "kube-dump-restorer-")
		if startErr != nil {
			return startErr
		}

		reportProgress(ctx, ProgressEvent{Stage: StageHelperReady, Percent: 20})
		reportProgress(ctx, ProgressEvent{Stage: StageStreaming, Percent: 25})

		result, operationErr := helperOptions.WaitCompleted(ctx, pod, r.options.CompletionTimeout)
		_ = result

		// Cleanup is independent from the transfer result
		// and is reported separately when the restore itself completed.
		cleanupContext, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		cleanupErr := helperOptions.Cleanup(cleanupContext, pod)
		cancel()

		if cleanupErr != nil {
			operationErr = errors.Join(operationErr, &CleanupWarning{
				Resource: fmt.Sprintf("restore Pod %s/%s", pod.Namespace, pod.Name),
				Err:      cleanupErr,
			})
		}

		if operationErr == nil {
			reportProgress(ctx, ProgressEvent{Stage: StageFinalizing, Percent: 90})
			return nil
		}
		if cleanupErr != nil || index+1 == len(agentBinaries) ||
			!errors.Is(operationErr, errHelperAgentStart) || ctx.Err() != nil {
			return operationErr
		}
	}

	return errors.New("S3 restorer has no helper binary")
}

// helperOptions builds the one-shot restore Pod command and its secret mounts.
func (r *S3Restorer) helperOptions(credentialSecret, identitySecret, passphraseSecret string) HelperPodOptions {
	arguments := []string{
		"volume",
		"download",
		r.options.URI,
		r.options.Object,
		"/data",
		string(r.options.Existing),
		"--compression=" + string(r.options.Compression),
		"--expected-size=" + strconv.FormatInt(r.options.ExpectedSize, 10),
		"--expected-sha256=" + r.options.ExpectedSHA256,
	}
	if r.options.Encrypted {
		arguments = append(arguments, "--encrypted")
	}
	if r.options.Endpoint != "" {
		arguments = append(arguments, "--s3-endpoint="+r.options.Endpoint)
	}
	if r.options.Insecure {
		arguments = append(arguments, "--s3-insecure")
	}
	if r.options.Region != "" {
		arguments = append(arguments, "--s3-region="+r.options.Region)
	}

	environmentSecrets := make([]string, 0, 3)
	if credentialSecret != "" {
		environmentSecrets = append(environmentSecrets, credentialSecret)
	}
	if passphraseSecret != "" {
		environmentSecrets = append(environmentSecrets, passphraseSecret)
	}

	secretVolumes := []HelperSecretVolume(nil)
	if identitySecret != "" {
		secretVolumes = []HelperSecretVolume{{
			SecretName: identitySecret,
			MountPath:  "/run/kube-dump",
		}}
	}

	return HelperPodOptions{
		Dynamic:                      r.options.Dynamic,
		Image:                        r.options.Image,
		ImagePullPolicy:              r.options.ImagePullPolicy,
		AgentBinary:                  r.options.AgentBinary,
		MountPath:                    "/data",
		ExpectedTargetIdentity:       r.options.ExpectedTargetIdentity,
		ExpectedTargetSize:           r.options.ExpectedTargetSize,
		RunID:                        r.options.RunID,
		Arguments:                    arguments,
		EnvironmentFromSecrets:       environmentSecrets,
		SecretVolumes:                secretVolumes,
		AutomountServiceAccountToken: credentialSecret == "",
	}
}

// createSecret creates a temporary Secret only when at least one value is present.
func (r *S3Restorer) createSecret(ctx context.Context, target Ref, purpose string, values map[string]string) (string, error) {
	if len(values) == 0 || allEmpty(values) {
		return "", nil
	}

	stringData := make(map[string]any, len(values))
	for key, value := range values {
		if value != "" {
			stringData[key] = value
		}
	}

	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"generateName": "kube-dump-" + purpose + "-",
			"labels":       temporaryResourceLabels(target, r.options.RunID),
		},
		"type":       "Opaque",
		"stringData": stringData,
	}}

	created, err := r.options.Dynamic.
		Resource(secretResource).
		Namespace(target.Namespace).
		Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create S3 restore %s Secret: %w", purpose, err)
	}

	return created.GetName(), nil
}

// deleteSecret removes one temporary restore Secret and tolerates prior cleanup.
func (r *S3Restorer) deleteSecret(namespace, name string) error {
	if name == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	if err := r.options.Dynamic.Resource(secretResource).
		Namespace(namespace).
		Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete S3 restore Secret %q: %w", name, err)
	}

	return nil
}

// allEmpty reports whether a secret value map contains only blank values.
func allEmpty(values map[string]string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}

	return true
}
