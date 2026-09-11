// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"io"

	"github.com/woozymasta/flags"
	"github.com/woozymasta/kube-dump/v2/internal/compress"
	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	nativegit "github.com/woozymasta/kube-dump/v2/internal/git"
	"github.com/woozymasta/kube-dump/v2/internal/kube"
	"github.com/woozymasta/kube-dump/v2/internal/logger"
	"github.com/woozymasta/kube-dump/v2/internal/progress"
	"github.com/woozymasta/kube-dump/v2/internal/pvc"
	corev1 "k8s.io/api/core/v1"
)

// Options describes the complete public command model.
type Options struct { // betteralign:ignore
	commandStreams

	ctx         context.Context       // ctx carries the process cancellation signal into command execution.
	environment flags.EnvironmentInfo // environment is the runtime terminal snapshot used by presentation features.

	Profile  ProfileCommand  `command:"profile"  description:"Manage profiles for selecting and cleaning Kubernetes objects"`
	Volume   VolumeCommand   `command:"volume"   description:"Run the temporary PVC volume helper" hidden:"true"`
	Key      KeyCommand      `command:"key"      description:"Generate age identities and maintain AES-SIV keyrings"`
	Image    ImageCommand    `command:"image"    description:"Capture, inspect, and publish images used by Kubernetes Pods"`
	Resource ResourceCommand `command:"resource" description:"Capture and process Kubernetes object manifests"`
	Pvc      PvcCommand      `command:"pvc"      description:"Capture and restore files stored in PersistentVolumeClaims"`

	Log            logger.Options `group:"Logging" namespace:"log" env-namespace:"LOG"`
	NetworkOptions `group:"Network"`
	OutputOptions  `group:"Output"`
}

// NetworkOptions contains application-wide network behavior settings.
type NetworkOptions struct {
	RetryAttempts int `default:"4" validate-min:"1" validate-max:"10" long:"retry-attempts" description:"Maximum attempts for a retryable network request, including the initial attempt"`
}

// OutputOptions contains application-wide output behavior settings.
type OutputOptions struct {
	NoProgress bool `long:"no-progress" description:"Disable terminal progress bars"`
}

// ImageCommand groups all operations over OCI image artifacts.
type ImageCommand struct {
	Push     ImagePushCommand     `command:"push"     description:"Publish a saved OCI image layout to a registry"`
	Inspect  InspectImagesCommand `command:"inspect"  description:"Inspect a saved OCI image layout"`
	Download ImageDownloadCommand `command:"download" description:"Download a saved OCI image layout from S3"`

	kube.ClientOptions
	Save ImagesCommand `command:"save" description:"Capture images referenced by Kubernetes Pods"`
}

// ImageDownloadCommand groups image layout download sources.
type ImageDownloadCommand struct {
	S3 ImageDownloadS3Command `command:"s3" description:"Download the images subtree from S3 into a local capture directory"`
}

// ImageDownloadS3Command downloads an OCI Image Layout from S3.
type ImageDownloadS3Command struct {
	commandContext

	Positional struct {
		Destination string `required:"true" positional-arg-name:"DESTINATION" description:"Local capture directory"`
	} `positional-args:"yes"`
	S3Destination `namespace:"s3" env-namespace:"S3"`
}

// InspectImagesCommand groups OCI Image Layout inspection sources.
type InspectImagesCommand struct {
	Dir InspectImagesDirCommand `command:"dir" description:"Inspect the images subtree in a local capture directory"`
	S3  InspectImagesS3Command  `command:"s3"  description:"Inspect an OCI image layout stored in S3"`
}

// InspectImagesDirCommand inspects a local OCI Image Layout.
type InspectImagesDirCommand struct {
	commandContext
	commandStreams
	ImageDirectoryPath `positional-args:"yes"`
}

// InspectImagesS3Command inspects an OCI Image Layout stored below S3 URI.
type InspectImagesS3Command struct {
	commandContext
	commandStreams
	S3Destination `namespace:"s3" env-namespace:"S3"`
}

// ImagePushCommand groups OCI Image Layout publishing sources.
type ImagePushCommand struct {
	Dir ImagePushDirCommand `command:"dir" description:"Publish the images subtree from a local capture directory"`
	S3  ImagePushS3Command  `command:"s3"  description:"Publish an OCI image layout from S3 to a registry"`
}

// ImagePushDirCommand publishes a local OCI Image Layout to a registry.
type ImagePushDirCommand struct {
	commandContext
	commandStreams

	Positional struct {
		Path   string   `positional-arg-name:"PATH"   description:"Source capture directory"                            required:"true"`
		Target string   `positional-arg-name:"TARGET" description:"Destination registry prefix"                         required:"true"`
		Images []string `positional-arg-name:"IMAGE"  description:"Optional saved image reference; omit to publish all"`
	} `positional-args:"yes"`
}

// ImagePushS3Command publishes an S3 OCI Image Layout to a registry.
type ImagePushS3Command struct {
	commandContext
	commandStreams
	S3Destination `namespace:"s3" env-namespace:"S3"`

	Positional struct {
		Target string   `positional-arg-name:"TARGET" description:"Destination registry prefix" required:"true"`
		Images []string `positional-arg-name:"IMAGE"  description:"Optional saved image reference; omit to publish all"`
	} `positional-args:"yes"`
}

// ImageTarget is the explicit registry prefix used by image publishing.
type ImageTarget struct {
	Target string `required:"true" positional-arg-name:"TARGET" description:"Destination registry prefix"`
}

// ImagesCommand groups container image save destinations.
type ImagesCommand struct {
	ImageOptions
	S3            ImagesS3Command `command:"s3"  description:"Save captured images below an S3 capture prefix"`
	clientOptions kube.ClientOptions
	Dir           ImagesDirCommand `command:"dir" description:"Save captured images below a local capture directory"`
}

// ImageOptions contains the Kubernetes scope and registry options shared by image save destinations.
type ImageOptions struct {
	ProfileOptions        `group:"Profile"`
	ImageSelectionOptions `group:"Selection"`
	ImageCaptureOptions   `group:"Images"`
}

// ImageSelectionOptions contains namespace filters for image discovery.
type ImageSelectionOptions struct {
	Namespaces []string `short:"n" env-delim:"," long:"namespace" description:"Include images used by Pods in these namespaces"`
}

// ImageCaptureOptions contains registry and platform settings for image capture.
type ImageCaptureOptions struct {
	RegistryMirrors map[string]string `long:"registry-mirror"    description:"Map a source registry host to a mirror as SOURCE=TARGET"     env-delim:"," key-value-delimiter:"="`
	Platforms       []string          `long:"image-platform"     description:"Capture only these OCI platforms, for example linux/amd64"   env-delim:","`
	PullSecrets     bool              `long:"image-pull-secrets" description:"Use Pod imagePullSecrets as additional registry credentials" auto-env:"false"`
}

// ImagePruneOptions controls cleanup of a local OCI image layout.
type ImagePruneOptions struct {
	Prune bool `long:"prune" auto-env:"false" description:"Remove saved image references and blobs not included by this capture"`
}

// ImageDirectoryPath is the destination directory for an OCI Image Layout.
type ImageDirectoryPath struct {
	Path string `required:"true" positional-arg-name:"PATH" description:"Destination capture directory"`
}

// ImagesDirCommand saves container images into a local OCI Image Layout.
type ImagesDirCommand struct {
	commandContext
	ImageDirectoryPath `positional-args:"yes"`
	imageOptions       ImageOptions
	clientOptions      kube.ClientOptions
	ImagePruneOptions
}

// ImagesS3Command saves container images into an S3-backed OCI layout.
type ImagesS3Command struct {
	commandContext
	S3Destination `namespace:"s3" env-namespace:"S3"`
	imageOptions  ImageOptions
	clientOptions kube.ClientOptions
}

// ProfileOptions selects the normalization policy used for resource output.
type ProfileOptions struct {
	ProfileDirectoryOptions
	Profile string `short:"p" long:"profile" default:"backup" description:"Profile name, builtin:name, global:name, or YAML file"`
}

// ProfileCommand groups profile inspection and validation commands.
type ProfileCommand struct {
	ProfileDirectoryOptions `group:"Profile"`

	List     ProfileListCommand     `command:"ls"       description:"List built-in and installed profiles"`
	Show     ProfileShowCommand     `command:"show"     description:"Print a profile as YAML"`
	Schema   ProfileSchemaCommand   `command:"schema"   description:"Print the JSON Schema for profile files"`
	Validate ProfileValidateCommand `command:"validate" description:"Validate a profile against the profile schema"`
	Which    ProfileWhichCommand    `command:"which"    description:"Show where a profile is stored"`
	Remove   ProfileRemoveCommand   `command:"rm"       description:"Remove an installed profile"`
	Edit     ProfileEditCommand     `command:"edit"     description:"Edit an installed profile"`
	Copy     ProfileCopyCommand     `command:"cp"       description:"Copy a built-in or file-based profile to the profile directory"`
	Install  ProfileInstallCommand  `command:"install"  description:"Validate and install a profile in the profile directory"`
}

// ProfileDirectoryOptions configures the directory containing global profiles.
type ProfileDirectoryOptions struct {
	Directory string `long:"profile-dir" description:"Directory containing global profiles"`
}

// ProfileCopyCommand copies a valid profile into the global catalog.
type ProfileCopyCommand struct {
	commandStreams
	commandContext

	Positional struct {
		Source string `required:"true" positional-arg-name:"SOURCE" description:"Profile name or YAML file"`
		Name   string `required:"true" positional-arg-name:"NAME"   description:"Global profile alias"`
	} `positional-args:"yes"`

	profileDirectory string
	Force            bool `description:"Replace an existing global profile" short:"f" long:"force" auto-env:"false"`
}

// ProfileEditCommand opens a global profile in the configured editor.
type ProfileEditCommand struct {
	commandContext

	Positional struct {
		Name string `required:"true" positional-arg-name:"NAME" description:"Global profile alias"`
	} `positional-args:"yes"`

	profileDirectory string
}

// ProfileInstallCommand validates and installs a profile into the global catalog.
type ProfileInstallCommand struct {
	commandStreams
	commandContext

	Positional struct {
		Source string `description:"Profile name or YAML file" required:"true" positional-arg-name:"SOURCE"`
	} `positional-args:"yes"`

	profileDirectory string
	Force            bool `description:"Replace an existing global profile" short:"f" long:"force" auto-env:"false"`
}

// ProfileListCommand lists profiles embedded in the binary.
type ProfileListCommand struct {
	commandContext
	commandStreams
	profileDirectory string
}

// ProfileRemoveCommand removes a profile from the global catalog.
type ProfileRemoveCommand struct {
	commandContext
	commandStreams

	Positional struct {
		Name string `required:"true" positional-arg-name:"NAME" description:"Global profile alias"`
	} `positional-args:"yes"`

	profileDirectory string
}

// ProfileShowCommand prints a built-in or file-based profile.
type ProfileShowCommand struct {
	commandContext
	commandStreams

	Positional struct {
		Name string `required:"true" positional-arg-name:"NAME" description:"Built-in profile name or YAML file"`
	} `positional-args:"yes"`

	profileDirectory string
}

// ProfileValidateCommand validates a built-in or file-based profile.
type ProfileValidateCommand struct {
	commandContext
	commandStreams

	Positional struct {
		Path string `required:"true" positional-arg-name:"PATH" description:"Profile name or YAML file"`
	} `positional-args:"yes"`

	profileDirectory string
}

// ProfileSchemaCommand prints the embedded JSON Schema for profile files.
type ProfileSchemaCommand struct {
	commandStreams
}

// ProfileWhichCommand prints the canonical source of a profile.
type ProfileWhichCommand struct {
	commandContext
	commandStreams

	Positional struct {
		Name string `required:"true" positional-arg-name:"NAME" description:"Profile name or file"`
	} `positional-args:"yes"`

	profileDirectory string
}

// KeyCommand groups age identity generation and AES-SIV keyring maintenance.
type KeyCommand struct {
	Generate KeyGenerateCommand `command:"generate" description:"Generate a native age identity"`
	Rotate   KeyRotateCommand   `command:"rotate"   description:"Rotate the active AES-SIV key"`
	Rewrap   KeyRewrapCommand   `command:"rewrap"   description:"Re-encrypt AES-SIV key envelopes"`
}

// KeyGenerateCommand generates a native age X25519 identity.
type KeyGenerateCommand struct {
	commandContext
	commandStreams

	KeyOutputOptions `group:"Output"`
}

// KeyOutputOptions contains the destination for generated key material.
type KeyOutputOptions struct {
	Output string `short:"o" long:"output" description:"Write the identity to this file; default: standard output"`
}

// KeyringMaintenanceOptions contains age credentials used to update a local keyring.
type KeyringMaintenanceOptions struct {
	IdentityOptions
	EncryptionOptions
}

// KeyRotateCommand creates a new active AES-SIV key in a local backup keyring.
type KeyRotateCommand struct {
	commandContext

	Positional struct {
		Directory string `required:"true" positional-arg-name:"DIRECTORY" description:"Local backup directory or Git worktree"`
	} `positional-args:"yes"`

	KeyringMaintenanceOptions `group:"Encryption"`
}

// KeyRewrapCommand replaces keyring envelopes with envelopes for new recipients.
type KeyRewrapCommand struct {
	commandContext

	Positional struct {
		Directory string `required:"true" positional-arg-name:"DIRECTORY" description:"Local backup directory or Git worktree"`
	} `positional-args:"yes"`

	KeyringMaintenanceOptions `group:"Encryption"`
}

// PvcCommand groups all operations over PVC filesystem artifacts.
type PvcCommand struct {
	Inspect  InspectPvcCommand     `command:"inspect"  description:"Inspect PVC archives stored locally or in S3"`
	Download PvcDownloadCommand    `command:"download" description:"Download PVC archives from S3"`
	Restore  PvcRestoreCommand     `command:"restore"  description:"Restore a saved PVC archive into an existing PVC"`
	Decrypt  DecryptArchiveCommand `command:"decrypt"  description:"Decrypt a local PVC archive encrypted with age"`
	Extract  PvcExtractCommand     `command:"extract"  description:"Restore files from a PVC archive"`
	Encrypt  EncryptArchiveCommand `command:"encrypt"  description:"Encrypt a local PVC archive with age"`

	kube.ClientOptions
	Save PvcSaveCommand `command:"save" description:"Capture PVC filesystems as compressed archives"`
}

// InspectPvcCommand groups PVC artifact inspection sources.
type InspectPvcCommand struct {
	Dir InspectPvcDirCommand `command:"dir" description:"Inspect PVC archives in a local directory"`
	S3  InspectPvcS3Command  `command:"s3"  description:"Inspect PVC archives stored in S3"`
}

// InspectPvcDirCommand inspects PVC artifacts in a local capture directory.
type InspectPvcDirCommand struct {
	commandContext
	commandStreams
	SourcePath `positional-args:"yes"`
}

// InspectPvcS3Command inspects PVC artifacts stored below an S3 URI.
type InspectPvcS3Command struct {
	commandContext
	commandStreams
	S3Destination `namespace:"s3" env-namespace:"S3"`
}

// PvcDownloadCommand groups PVC artifact download sources.
type PvcDownloadCommand struct {
	S3 PvcDownloadS3Command `command:"s3" description:"Download selected PVC archives from S3 to a local directory"`
}

// PvcDownloadS3Command downloads a PVC artifact prefix from S3.
type PvcDownloadS3Command struct {
	commandContext

	Positional struct {
		Destination string `positional-arg-name:"DESTINATION" description:"Local PVC artifact directory" required:"true"`
		Object      string `positional-arg-name:"OBJECT"      description:"Optional object key below URI"`
	} `positional-args:"yes"`
	S3Destination `namespace:"s3" env-namespace:"S3"`
}

// PvcExtractCommand extracts one PVC archive while preserving its metadata.
type PvcExtractCommand struct {
	commandContext

	Positional struct {
		Source      string `required:"true" positional-arg-name:"SOURCE"      description:"PVC archive path"`
		Destination string `required:"true" positional-arg-name:"DESTINATION" description:"Extraction directory"`
	} `positional-args:"yes"`

	IdentityOptions `group:"Encryption"`
}

// PvcRestoreCommand groups PVC restore sources.
type PvcRestoreCommand struct {
	Dir PvcRestoreDirCommand `command:"dir" description:"Restore a local PVC archive into an existing PVC"`
	S3  PvcRestoreS3Command  `command:"s3"  description:"Restore an S3 PVC archive into an existing PVC"`

	clientOptions kube.ClientOptions
}

// PvcHelperOptions contains helper Pod settings shared by PVC save and restore.
type PvcHelperOptions struct {
	Image            string            `long:"image"              description:"Container image used by the PVC helper Pod"`
	ImagePullPolicy  corev1.PullPolicy `long:"image-pull-policy"  description:"Pull policy for temporary PVC helper Pods" default:"IfNotPresent" choices:"Always;IfNotPresent;Never"`
	HelperBinaryPath string            `long:"helper-binary-path" description:"Path to the volume agent inside the helper container; default: kube-dump from PATH, then /kube-dump"`
}

// PvcRestoreOptions selects the saved artifact and target PVC.
type PvcRestoreOptions struct {
	PvcRestoreSelectionOptions `group:"Restore"`
	PvcHelperOptions           `group:"PVC"`
	IdentityOptions            `group:"Encryption"`
}

// PvcRestoreSelectionOptions identifies the source artifact and target PVC.
type PvcRestoreSelectionOptions struct {
	SourcePVC string             `long:"pvc"        description:"Saved source PVC as NAMESPACE/NAME" required:"true"`
	TargetPVC string             `long:"target-pvc" description:"Existing target PVC as NAMESPACE/NAME" required:"true"`
	Revision  string             `long:"revision"   description:"Artifact timestamp or latest" default:"latest"`
	Existing  pvc.ExistingPolicy `long:"existing"   description:"Policy for existing target data" default:"empty-only" choices:"empty-only;merge;replace"`
}

// PvcRestoreDirCommand restores a local PVC artifact into an existing PVC.
type PvcRestoreDirCommand struct {
	commandContext

	PvcArtifactDirectoryPath `positional-args:"yes"`
	PvcRestoreOptions

	clientOptions kube.ClientOptions
}

// PvcArtifactDirectoryPath is the positional local PVC artifact source.
type PvcArtifactDirectoryPath struct {
	Path string `required:"true" positional-arg-name:"PATH" description:"PVC artifact directory"`
}

// PvcRestoreS3Command restores an S3 PVC artifact into an existing PVC.
type PvcRestoreS3Command struct {
	commandContext
	S3Destination `namespace:"s3" env-namespace:"S3"`
	PvcRestoreOptions

	PvcMoverOptions `group:"S3"`

	clientOptions kube.ClientOptions
}

// PvcSaveCommand groups PVC save destinations.
type PvcSaveCommand struct {
	clientOptions kube.ClientOptions
	S3            PvcSaveS3Command  `command:"s3"  description:"Capture PVC filesystems and store archives in S3"`
	Dir           PvcSaveDirCommand `command:"dir" description:"Capture PVC filesystems and store archives locally"`
}

// PvcSaveOptions contains settings shared by all PVC save destinations.
type PvcSaveOptions struct {
	ProfileOptions    `group:"Profile"`
	EncryptionOptions `group:"Encryption"`
	PvcCaptureOptions `group:"PVC"`
}

// PvcCaptureOptions contains PVC selection, acquisition, and retention settings.
type PvcCaptureOptions struct {
	PvcHelperOptions

	Strategy         pvc.Strategy       `long:"strategy"           description:"PVC data acquisition strategy" default:"snapshot-copy" choices:"pod;snapshot-copy"`
	SnapshotClass    string             `long:"snapshot-class"     description:"VolumeSnapshotClass for snapshot-based strategies"`
	Compression      compress.Algorithm `long:"compression"        description:"Compression for portable PVC data" default:"zstd" choices:"zstd;gzip"`
	Namespaces       []string           `long:"namespace"          description:"Limit PVC data backup to these namespaces" env-delim:"," short:"n"`
	PVCs             []string           `long:"pvc"                description:"PVC name or glob pattern; repeatable" env-delim:","`
	Concurrency      int                `long:"concurrency"        description:"Maximum parallel PVC backup lifecycles" default:"4" min:"1"`
	MaxSize          pvc.Size           `long:"max-size"           description:"Skip an individual PVC above this size, for example 1GiB"`
	MaxNamespaceSize pvc.Size           `long:"max-namespace-size" description:"Skip PVC data in a namespace above this total, for example 10GiB"`
	Keep             int                `long:"keep"               description:"Keep this many latest artifacts for each PVC; 0 disables rotation" default:"5" min:"0"`
}

// PvcSaveDirCommand writes PVC data to a directory.
type PvcSaveDirCommand struct {
	commandContext
	DirectoryPath `positional-args:"yes"`
	clientOptions kube.ClientOptions
	PvcSaveOptions
}

// PvcSaveS3Command writes PVC data to an S3-compatible store.
type PvcSaveS3Command struct {
	commandContext
	S3Destination   `namespace:"s3" env-namespace:"S3"`
	PvcMoverOptions `group:"S3"`
	clientOptions   kube.ClientOptions
	PvcSaveOptions
}

// PvcMoverOptions contains the S3 endpoint used by a helper Pod.
type PvcMoverOptions struct {
	PodEndpoint string `long:"s3-pod-endpoint" description:"S3 endpoint reachable from the Kubernetes mover Pod"`
}

// ResourceCommand groups all operations over Kubernetes resource artifacts.
type ResourceCommand struct {
	kube.ClientOptions

	Download ResourceDownloadCommand `command:"download" description:"Copy manifests from Git, S3, or a local archive"`
	Cat      ResourceCatCommand      `command:"cat"      description:"Print manifests as YAML for review or kubectl"`
	Inspect  InspectResourceCommand  `command:"inspect"  description:"Inspect manifests and archives without applying them"`
	Extract  ResourceExtractCommand  `command:"extract"  description:"Restore manifests from a resource archive"`
	Encrypt  ResourceEncryptCommand  `command:"encrypt"  description:"Encrypt inline fields or a local archive"`
	Decrypt  ResourceDecryptCommand  `command:"decrypt"  description:"Decrypt inline fields or a local archive"`
	Save     ResourcesCommand        `command:"save"     description:"Collect selected Kubernetes objects into files or an archive"`
}

// ResourceCatCommand groups Kubernetes resource readers by source.
type ResourceCatCommand struct {
	S3        ResourceCatS3Command        `command:"s3"         description:"Print manifests from an S3 resource tree"`
	ArchiveS3 ResourceCatArchiveS3Command `command:"archive-s3" description:"Print manifests from a compressed S3 archive"`
	Dir       ResourceCatDirCommand       `command:"dir"        description:"Print manifests from a local directory"`
	Git       ResourceCatGitCommand       `command:"git"        description:"Print manifests from a Git worktree or remote"`
	Archive   ResourceCatArchiveCommand   `command:"archive"    description:"Print manifests from a local archive"`
}

// ResourceCatArchiveCommand reads a local compressed resource archive as YAML.
type ResourceCatArchiveCommand struct {
	commandContext
	commandStreams
	ArchiveSourcePath `positional-args:"yes"`
	IdentityOptions   `group:"Encryption"`
}

// ResourceCatArchiveS3Command reads one compressed resource archive from S3.
type ResourceCatArchiveS3Command struct {
	commandContext
	commandStreams
	S3Destination   `namespace:"s3" env-namespace:"S3"`
	IdentityOptions `group:"Encryption"`
}

// ResourceCatDirCommand reads a directory capture as Kubernetes YAML.
type ResourceCatDirCommand struct {
	commandContext
	commandStreams
	SourcePath      `positional-args:"yes"`
	IdentityOptions `group:"Encryption"`
}

// ResourceCatGitCommand reads a Git-backed resource capture as Kubernetes YAML.
type ResourceCatGitCommand struct {
	commandContext
	commandStreams
	SourcePath      `positional-args:"yes"`
	IdentityOptions `group:"Encryption"`
}

// ResourceCatS3Command reads an S3-backed resource capture as Kubernetes YAML.
type ResourceCatS3Command struct {
	commandContext
	commandStreams
	S3Destination   `namespace:"s3" env-namespace:"S3"`
	IdentityOptions `group:"Encryption"`
}

// ResourceDecryptCommand groups local resource decryption operations.
type ResourceDecryptCommand struct {
	Dir     DecryptResourceCommand `command:"dir"     description:"Decrypt inline fields in a local resource directory"`
	Archive DecryptArchiveCommand  `command:"archive" description:"Decrypt a local resource archive encrypted with age"`
}

// DecryptResourceCommand decrypts inline resource markers
// while retaining the original algorithm in a decrypted marker for a later encrypt operation.
type DecryptResourceCommand struct {
	commandContext

	Positional struct {
		Input  string `required:"true" positional-arg-name:"INPUT"  description:"Source resource directory"`
		Output string `required:"true" positional-arg-name:"OUTPUT" description:"Destination resource directory"`
	} `positional-args:"yes"`

	IdentityOptions `group:"Encryption"`
}

// DecryptArchiveCommand decrypts an age-encrypted archive.
type DecryptArchiveCommand struct {
	commandContext

	Positional struct {
		Input  string `required:"true" positional-arg-name:"INPUT"  description:"Encrypted archive path"`
		Output string `required:"true" positional-arg-name:"OUTPUT" description:"Decrypted archive path"`
	} `positional-args:"yes"`

	IdentityOptions `group:"Encryption"`
}

// ResourceDownloadCommand groups resource sources that can be copied locally.
type ResourceDownloadCommand struct {
	S3        ResourceDownloadS3Command        `command:"s3"         description:"Copy a resource tree from S3 to a local directory"`
	ArchiveS3 ResourceDownloadArchiveS3Command `command:"archive-s3" description:"Download one resource archive from S3"`
	Archive   ResourceDownloadArchiveCommand   `command:"archive"    description:"Copy a local resource archive to another path"`
	Git       ResourceDownloadGitCommand       `command:"git"        description:"Copy a Git worktree or remote to local storage"`
}

// ResourceDownloadArchiveCommand copies a local archive to another path.
type ResourceDownloadArchiveCommand struct {
	commandContext

	Positional struct {
		Source      string `required:"true" positional-arg-name:"SOURCE"      description:"Source archive path"`
		Destination string `required:"true" positional-arg-name:"DESTINATION" description:"Local archive path"`
	} `positional-args:"yes"`
}

// ResourceDownloadArchiveS3Command downloads one archive object from S3.
type ResourceDownloadArchiveS3Command struct {
	commandContext

	Positional struct {
		Destination string `required:"true" description:"Local archive path" positional-arg-name:"DESTINATION"`
	} `positional-args:"yes"`

	S3Destination `namespace:"s3" env-namespace:"S3"`
}

// ResourceDownloadGitCommand downloads a Git worktree or clones a Git remote.
type ResourceDownloadGitCommand struct {
	commandContext

	Positional struct {
		Source      string `required:"true" positional-arg-name:"SOURCE"      description:"Git URL or local worktree"`
		Destination string `required:"true" positional-arg-name:"DESTINATION" description:"Local resource directory"`
	} `positional-args:"yes"`

	Git nativegit.Options `group:"Git" namespace:"git" env-namespace:"GIT"`
}

// ResourceDownloadS3Command downloads a resource prefix from S3.
type ResourceDownloadS3Command struct {
	commandContext

	Positional struct {
		Destination string `positional-arg-name:"DESTINATION" description:"Local resource directory" required:"true"`
		Object      string `positional-arg-name:"OBJECT"      description:"Optional object key below URI"`
	} `positional-args:"yes"`

	S3Destination `namespace:"s3" env-namespace:"S3"`
}

// ResourceEncryptCommand groups local resource encryption operations.
type ResourceEncryptCommand struct {
	Dir     EncryptResourceCommand `command:"dir"     description:"Encrypt inline fields in a local resource directory"`
	Archive EncryptArchiveCommand  `command:"archive" description:"Encrypt a local resource archive with age"`
}

// EncryptResourceCommand encrypts decrypted inline resource markers.
type EncryptResourceCommand struct {
	commandContext
	IdentityOptions `group:"Encryption"`

	Positional struct {
		Input  string `required:"true" positional-arg-name:"INPUT"  description:"Source resource directory"`
		Output string `required:"true" positional-arg-name:"OUTPUT" description:"Destination resource directory"`
	} `positional-args:"yes"`

	ArchiveOptions
}

// EncryptArchiveCommand encrypts an existing archive with age.
type EncryptArchiveCommand struct {
	commandContext

	Positional struct {
		Input  string `required:"true" positional-arg-name:"INPUT"  description:"Input archive path"`
		Output string `required:"true" positional-arg-name:"OUTPUT" description:"Encrypted archive path"`
	} `positional-args:"yes"`

	ArchiveOptions
}

// ResourceExtractCommand groups resource archive extraction sources.
type ResourceExtractCommand struct {
	Archive   ResourceExtractArchiveCommand   `command:"archive"    description:"Restore manifests from a local archive"`
	ArchiveS3 ResourceExtractArchiveS3Command `command:"archive-s3" description:"Download and restore manifests from an S3 archive"`
}

// ResourceExtractArchiveCommand extracts one local resource archive.
type ResourceExtractArchiveCommand struct {
	commandContext

	Positional struct {
		Source      string `required:"true" positional-arg-name:"SOURCE"      description:"Source archive path"`
		Destination string `required:"true" positional-arg-name:"DESTINATION" description:"Extraction directory"`
	} `positional-args:"yes"`

	IdentityOptions `group:"Encryption"`
}

// ResourceExtractArchiveS3Command extracts one archive object from S3.
type ResourceExtractArchiveS3Command struct {
	commandContext
	S3Destination `namespace:"s3" env-namespace:"S3"`

	Positional struct {
		Destination string `required:"true" positional-arg-name:"DESTINATION" description:"Extraction directory"`
	} `positional-args:"yes"`

	IdentityOptions `group:"Encryption"`
}

// InspectResourceCommand groups Kubernetes resource inspection sources.
type InspectResourceCommand struct {
	Dir       InspectDirCommand     `command:"dir"        description:"Show metadata and counts for a local directory"`
	Git       InspectGitCommand     `command:"git"        description:"Show metadata and counts for a Git worktree"`
	S3        InspectS3Command      `command:"s3"         description:"Show metadata and counts for an S3 resource tree"`
	Archive   InspectArchiveCommand `command:"archive"    description:"Show metadata and counts for a local archive"`
	ArchiveS3 InspectS3Command      `command:"archive-s3" description:"Show metadata and counts for an S3 archive"`
}

// InspectArchiveCommand prints archive metadata and resource counts.
type InspectArchiveCommand struct {
	commandContext
	commandStreams

	Positional struct {
		Path string `description:"Archive path" required:"true" positional-arg-name:"PATH"`
	} `positional-args:"yes"`

	IdentityOptions `group:"Encryption"`
}

// InspectDirCommand inspects a local capture directory or archive file.
type InspectDirCommand struct {
	commandContext
	commandStreams
	SourcePath `positional-args:"yes"`
}

// InspectGitCommand inspects a capture stored in a Git worktree.
type InspectGitCommand struct {
	commandContext
	commandStreams
	SourcePath `positional-args:"yes"`
}

// InspectS3Command inspects a capture stored below an S3 URI.
type InspectS3Command struct {
	commandContext
	commandStreams
	S3Destination   `namespace:"s3" env-namespace:"S3"`
	IdentityOptions `group:"Encryption"`
}

// ResourcesCommand groups Kubernetes resource save destinations.
type ResourcesCommand struct {
	kube.Selection `group:"Selection"`
	ResourceOptions

	clientOptions kube.ClientOptions

	ArchiveS3 ResourcesArchiveS3Command `command:"archive-s3" description:"Capture manifests into one archive in S3"`
	Archive   ResourcesArchiveCommand   `command:"archive"    description:"Capture manifests into one local archive"`
	Stdout    ResourcesStdoutCommand    `command:"stdout"     description:"Capture manifests as multi-document YAML on standard output"`
	Git       ResourcesGitCommand       `command:"git"        description:"Capture manifests and commit them to a Git worktree"`
	S3        ResourcesS3Command        `command:"s3"         description:"Capture manifests and upload them to S3"`
	Dir       ResourcesDirCommand       `command:"dir"        description:"Capture manifests into a local directory"`
}

// ResourcePruneOptions controls optional cleanup of files from the previous resource backup.
// Without this option resource backups never remove objects written by an earlier run.
type ResourcePruneOptions struct {
	Prune bool `long:"prune" auto-env:"false" description:"Remove stale files superseded by this successful resource backup"`
}

// ResourcesStdoutCommand writes selected Kubernetes objects as multi-document YAML.
type ResourcesStdoutCommand struct {
	commandContext
	commandStreams
	selection       kube.Selection
	clientOptions   kube.ClientOptions
	resourceOptions ResourceOptions
}

// ResourceOptions contains output, encryption,
// and collection settings shared by resource backup destinations.
// Resource selection and Kubernetes client settings belong to their respective parent command groups.
type ResourceOptions struct {
	ProfileOptions            `group:"Profile"`
	ResourceEncryptionOptions `group:"Encryption"`
	kube.CollectionOptions    `group:"Collection"`
}

// ResourceEncryptionOptions contains field encryption settings
// and the age credentials used by resource capture and processing commands.
type ResourceEncryptionOptions struct {
	IdentityOptions
	FieldEncryption fieldcrypto.Mode `default:"age" choices:"age;aes-siv" long:"field-encryption" description:"Algorithm for fields selected by profile rules; no keys means plaintext"`
	EncryptionOptions
}

// ArchiveFormat controls whole-archive output encoding.
type ArchiveFormat string

// Archive format
const (
	FilesFormat      ArchiveFormat = "files"
	TarGZIPFormat    ArchiveFormat = "tar.gz"
	TarGZIPAgeFormat ArchiveFormat = "tar.gz.age"
	TarZSTDFormat    ArchiveFormat = "tar.zst"
	TarZSTDAgeFormat ArchiveFormat = "tar.zst.age"
)

// ResourceArchiveOptions contains settings used only
// when resources are packed into one local or S3 archive object.
type ResourceArchiveOptions struct {
	ResourceArchiveFormatOptions     `group:"Archive"`
	ResourceArchiveEncryptionOptions `group:"Archive Encryption"`
}

// ResourceArchiveFormatOptions contains whole-archive encoding settings.
type ResourceArchiveFormatOptions struct {
	Format ArchiveFormat `short:"f" long:"format" default:"tar.zst" description:"Compression and encryption format" choices:"tar.gz;tar.gz.age;tar.zst;tar.zst.age"`
}

// ResourceArchiveEncryptionOptions contains recipients for whole-archive encryption.
type ResourceArchiveEncryptionOptions struct {
	ArchiveRecipients           []string `env-delim:"," long:"archive-recipient"             description:"Age recipient for whole-archive encryption"`
	ArchiveRecipientsFiles      []string `env-delim:"," long:"archive-recipients-file"       description:"File containing age recipients for whole-archive encryption"`
	ArchiveRecipientsURLs       []string `env-delim:"," long:"archive-recipients-url"        description:"HTTP or HTTPS URL containing recipients for whole-archive encryption"`
	ArchiveRecipientGitHubUsers []string `env-delim:"," long:"archive-recipient-github-user" description:"GitHub username whose public SSH keys encrypt the whole archive"`
}

// ResourceDestinationPath is the destination path for a resource directory.
type ResourceDestinationPath struct {
	Path string `required:"true" positional-arg-name:"PATH" description:"Destination directory"`
}

// ArchiveDestinationPath is the local path of a resource archive.
type ArchiveDestinationPath struct {
	Path string `required:"true" positional-arg-name:"PATH" description:"Destination archive path"`
}

// DirectoryPath is a local destination directory for PVC artifacts.
type DirectoryPath struct {
	Path string `required:"true" positional-arg-name:"PATH" description:"Destination directory"`
}

// GitWorktreePath is the local Git worktree used by resource storage.
type GitWorktreePath struct {
	Path string `required:"true" positional-arg-name:"PATH" description:"Git worktree directory"`
}

// SourcePath is the positional directory or worktree source accepted by
// resource readers that read loose resource files.
type SourcePath struct {
	Path string `required:"true" positional-arg-name:"PATH" description:"Source directory or Git worktree"`
}

// ArchiveSourcePath is the positional source accepted by archive readers.
type ArchiveSourcePath struct {
	Path string `required:"true" positional-arg-name:"PATH" description:"Source archive path"`
}

// GitDestination contains the destination path and Git persistence settings.
type GitDestination struct {
	GitWorktreePath
	Git nativegit.Options `group:"Git" namespace:"git" env-namespace:"GIT"`
}

// S3Destination contains the object-store destination settings.
type S3Destination struct {
	URI           string `long:"s3-uri"             description:"S3 destination URI" required:"true"`
	Endpoint      string `long:"s3-endpoint"        description:"S3-compatible API endpoint"`
	Region        string `long:"s3-region"          description:"S3 region"`
	Bucket        string `long:"s3-bucket"          description:"S3 bucket name"`
	Prefix        string `long:"s3-prefix"          description:"S3 object prefix"`
	AccessKey     string `long:"s3-access-key"      description:"S3 access key"`
	SecretKeyFile string `long:"s3-secret-key-file" description:"Read the S3 secret key from this file"`
	Insecure      bool   `long:"s3-insecure"        description:"Allow plaintext HTTP or loopback S3 endpoints"`
}

// ResourcesArchiveCommand writes selected Kubernetes objects to one archive.
type ResourcesArchiveCommand struct {
	commandContext
	ArchiveDestinationPath `positional-args:"yes"`
	ResourceArchiveOptions

	selection       kube.Selection
	clientOptions   kube.ClientOptions
	resourceOptions ResourceOptions
}

// ResourcesArchiveS3Command writes selected Kubernetes objects to one S3 archive object.
type ResourcesArchiveS3Command struct {
	commandContext
	S3Destination `namespace:"s3" env-namespace:"S3"`
	ResourceArchiveOptions

	selection       kube.Selection
	clientOptions   kube.ClientOptions
	resourceOptions ResourceOptions
}

// ResourcesDirCommand writes selected Kubernetes objects to a directory.
type ResourcesDirCommand struct {
	commandContext
	ResourceDestinationPath `positional-args:"yes"`

	selection       kube.Selection
	clientOptions   kube.ClientOptions
	resourceOptions ResourceOptions
	ResourcePruneOptions
}

// ResourcesGitCommand writes selected Kubernetes objects to a Git repository.
type ResourcesGitCommand struct {
	commandContext
	GitWorktreePath `positional-args:"yes"`

	Git             nativegit.Options `group:"Git" namespace:"git" env-namespace:"GIT"`
	selection       kube.Selection
	clientOptions   kube.ClientOptions
	resourceOptions ResourceOptions
	ResourcePruneOptions
}

// ResourcesS3Command writes selected Kubernetes objects to an S3-compatible store.
type ResourcesS3Command struct {
	commandContext
	S3Destination `namespace:"s3" env-namespace:"S3"`

	selection       kube.Selection
	clientOptions   kube.ClientOptions
	resourceOptions ResourceOptions
	ResourcePruneOptions
}

// IdentityOptions configures private age identities used to decrypt archives,
// encrypted resource fields, and AES-SIV keyrings.
type IdentityOptions struct {
	IdentityPassphrase     string   `long:"identity-passphrase"      description:"Passphrase for an encrypted SSH identity"                         xor:"identity-passphrase-file" secret:"true"`
	IdentityPassphraseFile string   `long:"identity-passphrase-file" description:"Read the passphrase for an encrypted SSH identity from this file" xor:"identity-passphrase"`
	IdentityFiles          []string `long:"identity"                 description:"Age identity file used to decrypt archives, fields, or keyrings" short:"i" env-delim:","`
}

// EncryptionOptions contains age recipient sources shared by encryption commands.
// IdentityOptions stays separate because saving data needs recipients,
// while decryption and keyring maintenance additionally need private identities.
type EncryptionOptions struct {
	Recipients           []string `env-delim:"," long:"recipient"             description:"Age recipient used for encryption" short:"R"`
	RecipientsFiles      []string `env-delim:"," long:"recipients-file"       description:"File containing age recipients"`
	RecipientsURLs       []string `env-delim:"," long:"recipients-url"        description:"HTTP or HTTPS URL containing age recipients"`
	RecipientGitHubUsers []string `env-delim:"," long:"recipient-github-user" description:"GitHub username whose public SSH keys provide age recipients"`
}

// ArchiveOptions contains recipients used by the standalone archive encryptor.
type ArchiveOptions struct {
	EncryptionOptions `group:"Archive Encryption"`
}

// VolumeCommand is a hidden helper protocol used by temporary PVC Pods.
type VolumeCommand struct {
	Hold     VolumeHoldCommand     `command:"hold"     description:"Keep a helper volume process alive"`
	Import   VolumeImportCommand   `command:"import"   description:"Import a mounted volume"`
	Export   VolumeExportCommand   `command:"export"   description:"Export a mounted volume"`
	Upload   VolumeUploadCommand   `command:"upload"   description:"Upload a mounted volume archive directly to S3"`
	Download VolumeDownloadCommand `command:"download" description:"Download an S3 archive into a mounted volume"`
}

// VolumeExportCommand streams a mounted filesystem archive to stdout.
type VolumeExportCommand struct {
	commandContext
	commandStreams

	Positional struct {
		Path string `default:"/volume" positional-arg-name:"PATH" description:"Mounted volume root"`
	} `positional-args:"yes"`
}

// VolumeImportCommand extracts a filesystem archive from stdin.
type VolumeImportCommand struct {
	commandContext
	commandStreams

	Positional struct {
		Path     string `default:"/volume"    positional-arg-name:"PATH"   description:"Mounted volume root"`
		Existing string `default:"empty-only" positional-arg-name:"POLICY" description:"Policy for existing data" choices:"empty-only;merge;replace"`
	} `positional-args:"yes"`
}

// VolumeUploadCommand streams a mounted filesystem archive directly to S3.
// It is an internal helper command invoked by a temporary mover Pod.
type VolumeUploadCommand struct {
	commandContext
	commandStreams

	Positional struct {
		URI    string `positional-arg-name:"URI"    description:"S3 destination URI" required:"true"`
		Object string `positional-arg-name:"OBJECT" description:"Relative PVC artifact directory" required:"true"`
		Path   string `positional-arg-name:"PATH"   description:"Mounted volume root" default:"/volume"`
		Result string `positional-arg-name:"RESULT" description:"Result file path" default:"/dev/termination-log"`
	} `positional-args:"yes"`

	VolumeUploadS3Options         `group:"S3"`
	VolumeUploadPVCOptions        `group:"PVC"`
	VolumeUploadEncryptionOptions `group:"Encryption"`
}

// VolumeUploadS3Options contains S3 settings for the internal upload helper.
type VolumeUploadS3Options struct {
	Endpoint string `auto-env:"false" long:"s3-endpoint" description:"S3-compatible API endpoint"`
	Region   string `auto-env:"false" long:"s3-region"   description:"S3 region"`
	Insecure bool   `auto-env:"false" long:"s3-insecure" description:"Allow plaintext HTTP or loopback S3 endpoints"`
}

// VolumeUploadPVCOptions contains archive settings for the internal upload helper.
type VolumeUploadPVCOptions struct {
	Compression compress.Algorithm `auto-env:"false" long:"compression" default:"zstd"          choices:"zstd;gzip"         description:"PVC archive compression"`
	Strategy    pvc.Strategy       `auto-env:"false" long:"strategy"    default:"snapshot-copy" choices:"pod;snapshot-copy" description:"PVC backup strategy"`
}

// VolumeUploadEncryptionOptions contains encryption settings for the internal upload helper.
type VolumeUploadEncryptionOptions struct {
	Recipients []string `auto-env:"false" long:"recipient" description:"Age recipient for PVC archive encryption"`
}

// VolumeDownloadCommand restores one S3 PVC archive inside a helper Pod.
// It is an internal command invoked by the direct S3 restore coordinator.
type VolumeDownloadCommand struct {
	commandStreams

	commandContext

	Positional struct {
		URI      string             `positional-arg-name:"URI"    description:"S3 source URI" required:"true"`
		Object   string             `positional-arg-name:"OBJECT" description:"S3 archive object key" required:"true"`
		Path     string             `positional-arg-name:"PATH"   description:"Mounted volume root" default:"/volume"`
		Existing pvc.ExistingPolicy `positional-arg-name:"POLICY" description:"Policy for existing data" default:"empty-only" choices:"empty-only;merge;replace"`
	} `positional-args:"yes"`

	VolumeDownloadS3Options         `group:"S3"`
	VolumeDownloadPVCOptions        `group:"PVC"`
	VolumeDownloadEncryptionOptions `group:"Encryption"`
	VolumeValidationOptions         `group:"Validation"`
}

// VolumeDownloadS3Options contains S3 settings for the internal download helper.
type VolumeDownloadS3Options struct {
	Endpoint string `auto-env:"false" long:"s3-endpoint" description:"S3-compatible API endpoint"`
	Region   string `auto-env:"false" long:"s3-region"   description:"S3 region"`
	Insecure bool   `auto-env:"false" long:"s3-insecure" description:"Allow plaintext HTTP or loopback S3 endpoints"`
}

// VolumeDownloadPVCOptions contains archive settings for the internal download helper.
type VolumeDownloadPVCOptions struct {
	Compression compress.Algorithm `auto-env:"false" long:"compression" description:"PVC archive compression" choices:"zstd;gzip"`
}

// VolumeDownloadEncryptionOptions contains decryption settings for the internal download helper.
type VolumeDownloadEncryptionOptions struct {
	IdentityPath       string `long:"identity-path" description:"Mounted age identity path" auto-env:"false" default:"/run/kube-dump/identity"`
	IdentityPassphrase string `long:"identity-passphrase" description:"Passphrase for an encrypted SSH identity" secret:"true"`
	Encrypted          bool   `long:"encrypted" description:"The S3 object is age-encrypted" auto-env:"false"`
}

// VolumeValidationOptions contains expected payload metadata for the internal download helper.
type VolumeValidationOptions struct {
	ExpectedSHA256 string `long:"expected-sha256" description:"Expected uncompressed payload SHA-256" auto-env:"false"`
	ExpectedSize   int64  `long:"expected-size" description:"Expected uncompressed payload size" auto-env:"false"`
}

// VolumeHoldCommand keeps the helper volume process alive.
type VolumeHoldCommand struct {
	commandContext
}

// commandStreams holds process input and output streams used by commands.
// It is bound once by the application runner
// and embedded only where a command actually reads stdin or writes user-facing output.
type commandStreams struct {
	output io.Writer
	input  io.Reader
	stderr io.Writer
}

// setStreams injects process streams into the command selected by the parser.
func (s *commandStreams) setStreams(output io.Writer, input io.Reader, stderr io.Writer) {
	s.output = output
	s.input = input
	s.stderr = stderr
}

// commandContext carries the process cancellation signal into commands.
type commandContext struct {
	ctx      context.Context
	progress *progress.Manager
}

// setContext installs the process context before command execution.
func (c *commandContext) setContext(ctx context.Context) {
	c.ctx = ctx
}

// setProgress installs the process progress manager before command execution.
func (c *commandContext) setProgress(manager *progress.Manager) {
	c.progress = manager
}

// context returns the injected process context for command execution.
func (c *commandContext) context() context.Context {
	if c.ctx == nil {
		return context.Background()
	}

	return c.ctx
}
