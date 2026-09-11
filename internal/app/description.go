// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"fmt"

	"github.com/woozymasta/flags"
)

// configureDescriptions installs user-facing command descriptions and examples.
func configureDescriptions(parser *flags.Parser, options *Options) error {
	var descriptionErr error
	applyDescription := func(parser *flags.Parser, target any, short, long string, examples ...*flags.CommandExample) {
		if descriptionErr != nil {
			return
		}
		descriptionErr = setCommandDescription(parser, target, short, long, examples...)
	}

	parser.SetLongDescription(`{{.ProgramBaseName}} saves Kubernetes resources, PVC data, and container images.

Use resource, pvc, and image commands to work with each artifact type.
Profiles control resource selection and cleanup; encryption is applied only when explicitly configured.`)

	applyDescription(parser, &options.Profile,
		"Manage profiles for selecting and cleaning Kubernetes objects",
		"Profiles define which Kubernetes objects are captured and which generated "+
			"or runtime fields are removed before the manifests are saved or exported.",
		flags.Example().Arg("ls"))
	applyDescription(parser, &options.Profile.Schema,
		"Print the JSON Schema for profile files",
		"Write the embedded JSON Schema used to validate profile files. "+
			"The output is intended for editors, CI validation, and other JSON Schema-aware tools.",
		flags.Example().Raw(">").Raw("profile.schema.json"))

	applyDescription(parser, &options.Key,
		"Generate age identities and maintain AES-SIV keyrings",
		"Create a native age identity or maintain the AES-SIV keyring stored inside a local backup directory.\n\n"+
			"Keyring maintenance works only with an existing local directory or Git worktree. "+
			"It does not access S3, rewrite resource manifests, or re-encrypt existing resource fields automatically. "+
			"Use download or extract first when the keyring is stored in a remote or archive backend.\n\n"+
			"The generate command creates only a native age X25519 identity. "+
			"Rotate changes the active AES-SIV key and keeps historical keys. "+
			"Rewrap changes only the age protection around existing AES-SIV keys. "+
			"Both maintenance commands require an authorized private identity and at least one new age recipient.",
		flags.Example().Arg("generate"))

	applyDescription(parser, &options.Key.Generate,
		"Generate a native age X25519 identity",
		"Create a native age X25519 identity for encrypting resource fields, archives, and AES-SIV keyrings.\n\n"+
			"The private identity is written to standard output unless `--output` is specified. "+
			"The output file is created with private-file permissions and is never overwritten. "+
			"This command does not create SSH identities, AES-SIV keyrings, or passphrase-protected identity files.",
		flags.Example().Option(&options.Key.Generate.Output, "age-identity.txt"))

	applyDescription(parser, &options.Key.Rotate,
		"Rotate the active AES-SIV key in a local keyring",
		"Create a new AES-SIV data key and make it active in an existing local backup keyring.\n\n"+
			"The current private age identity is required to open the keyring, "+
			"and recipient flags supply the age recipients for the new envelope. "+
			"Historical AES-SIV keys remain in the keyring, so existing encrypted values stay readable. "+
			"Resource manifests are not rewritten; use resource encrypt when that is required.\n\n"+
			"The DIRECTORY must contain `.kube-dump/crypto/metadata.yaml`. "+
			"Remote S3 objects and standalone archives must be downloaded or extracted to a local directory before this command can use them.",
		flags.Example().
			Arg("./backup").
			ShortOption(&options.Key.Rotate.IdentityFiles, "age-identity.txt").
			ShortOption(&options.Key.Rotate.Recipients, "age1example"))

	applyDescription(parser, &options.Key.Rewrap,
		"Re-encrypt AES-SIV key envelopes for new age recipients",
		"Replace every age-encrypted AES-SIV key envelope in an existing local backup keyring.\n\n"+
			"The AES-SIV keys and encrypted resource values do not change. "+
			"Only their age envelopes are rewritten for the recipients supplied by recipient flags. "+
			"The current private age identity is required to read all existing envelopes, including historical keys retained after rotation.\n\n"+
			"The DIRECTORY must contain `.kube-dump/crypto/metadata.yaml`. "+
			"This command does not modify resource manifests or operate directly on S3 and archive backends.",
		flags.Example().
			Arg("./backup").
			ShortOption(&options.Key.Rewrap.IdentityFiles, "age-identity.txt").
			ShortOption(&options.Key.Rewrap.Recipients, "age1example"))

	applyDescription(parser, &options.Resource,
		"Capture and process Kubernetes object manifests",
		"Collect selected Kubernetes objects into a directory or archive, read them as YAML, "+
			"inspect them, and move or transform the resulting manifest set.",
		flags.Example().Arg("save").Arg("dir").Arg("./backup"))
	applyDescription(parser, &options.Resource.Save,
		"Capture selected Kubernetes objects to stdout, files, or an archive",
		"Collect selected Kubernetes objects, apply the selected profile, "+
			"and write multi-document YAML to stdout or store canonical resource files "+
			"or an archive.",
		flags.Example().Arg("dir").Arg("./backup").Option(&options.Resource.Save.Profile, "backup"))
	applyDescription(parser, &options.Resource.Save.Stdout,
		"Capture selected Kubernetes objects as YAML on standard output",
		"Collect selected Kubernetes objects, apply the selected profile, and write each object as a YAML document. "+
			"A comment before every document shows its backup path.",
		flags.Example().
			Option(&options.Resource.Save.Profile, "backup").
			Raw("|").Raw("kubectl").Raw("apply").Raw("-f").Raw("-"))
	applyDescription(parser, &options.Resource.Cat,
		"Print resource manifests as YAML for review or kubectl",
		"Read saved resource files or an archive, decode inline fields, "+
			"and write Kubernetes YAML to standard output.",
		flags.Example().
			Arg("dir").Arg("./backup").Raw("|").Raw("kubectl").Raw("apply").Raw("-f").Raw("-"))
	applyDescription(parser, &options.Resource.Inspect,
		"Inspect manifests and archives without applying them",
		"Summarize saved Kubernetes resource files and archives without applying them.",
		flags.Example().Arg("dir").Arg("./backup"))
	applyDescription(parser, &options.Resource.Download,
		"Copy manifests from Git, S3, or a local archive",
		"Copy a resource dump from Git or S3, or copy an archive to a local path.",
		flags.Example().Arg("s3").Arg("./backup").Raw("--s3-uri").Raw("s3://bucket/backup"))
	applyDescription(parser, &options.Resource.Encrypt,
		"Encrypt inline fields or a local archive",
		"Encrypt inline resource fields or wrap a local resource archive with age.",
		flags.Example().Arg("dir").Arg("./plain").Arg("./encrypted"))
	applyDescription(parser, &options.Resource.Decrypt,
		"Decrypt inline fields or a local archive",
		"Decrypt inline resource fields or unwrap a local resource archive.",
		flags.Example().Arg("dir").Arg("./encrypted").Arg("./plain"))
	applyDescription(parser, &options.Resource.Extract,
		"Restore manifests from a resource archive",
		"Decrypt and unpack a local or S3 resource archive into a directory.",
		flags.Example().Arg("archive").Arg("backup.tar.zst").Arg("./backup"))

	applyDescription(parser, &options.Pvc,
		"Capture and restore files stored in PersistentVolumeClaims",
		"Read PVC filesystems through Kubernetes, store each claim as a portable compressed archive, "+
			"and inspect, transfer, encrypt, decrypt, or extract those archives.",
		flags.Example().Arg("save").Arg("dir").Arg("./pvc-data"))
	applyDescription(parser, &options.Pvc.Save,
		"Capture PVC filesystems as compressed archives",
		"Read selected PVC filesystems through the configured strategy and store one compressed artifact per claim in a new timestamped directory. "+
			"Artifacts are grouped by namespace and PVC; keep the latest per PVC with `--keep`. Use `--keep` 0 to disable rotation.",
		flags.Example().Arg("dir").Arg("./pvc-data"))
	applyDescription(parser, &options.Pvc.Download,
		"Download PVC archives from S3",
		"Copy PVC artifacts from an S3 prefix into a local artifact directory.",
		flags.Example().Arg("s3").Arg("./pvc-data").Raw("--s3-uri").Raw("s3://bucket/cluster/pvc/"))
	applyDescription(parser, &options.Pvc.Restore,
		"Restore a saved PVC archive into an existing PVC",
		"Restore one selected PVC artifact into an already created target PVC. "+
			"The target must be Bound, use Filesystem mode, be unused, and have capacity at least as large as the uncompressed archive payload. "+
			"The default empty-only policy refuses to overwrite existing data; use `--existing` merge or replace explicitly.",
		flags.Example().Arg("dir").Arg("./pvc-data"))
	applyDescription(parser, &options.Pvc.Restore.Dir,
		"Restore a local PVC archive into an existing PVC",
		"Select a saved PVC artifact by source identity and revision, then stream its decrypted and decompressed contents into the target PVC.",
		flags.Example().
			Arg("./pvc-data").
			Option(&options.Pvc.Restore.Dir.SourcePVC, "production/postgres-data").
			Option(&options.Pvc.Restore.Dir.TargetPVC, "recovery/postgres-data"))
	applyDescription(parser, &options.Pvc.Restore.S3,
		"Restore an S3 PVC archive into an existing PVC",
		"Restore a selected PVC artifact stored in S3. The decrypted and decompressed archive is streamed into the target helper Pod.",
		flags.Example().
			Raw("--s3-uri").Raw("s3://bucket/pvc").
			Option(&options.Pvc.Restore.S3.SourcePVC, "production/postgres-data").
			Option(&options.Pvc.Restore.S3.TargetPVC, "recovery/postgres-data"))
	applyDescription(parser, &options.Pvc.Inspect,
		"Inspect PVC archives stored locally or in S3",
		"Count PVC artifacts and report their stored size without extracting their contents.",
		flags.Example().Arg("dir").Arg("./pvc-data"))
	applyDescription(parser, &options.Pvc.Encrypt,
		"Encrypt a local PVC archive with age",
		"Wrap a local PVC archive with age and write it to another path.",
		flags.Example().Arg("backup.tar.zst").Arg("backup.tar.zst.age"))
	applyDescription(parser, &options.Pvc.Decrypt,
		"Decrypt a local PVC archive encrypted with age",
		"Remove age encryption from a local PVC archive.",
		flags.Example().Arg("backup.tar.zst.age").Arg("backup.tar.zst"))
	applyDescription(parser, &options.Pvc.Extract,
		"Restore files from a PVC archive",
		"Decrypt when necessary and restore the archive contents into a directory.",
		flags.Example().Arg("backup.tar.zst").Arg("./pvc"))

	applyDescription(parser, &options.Image,
		"Capture, inspect, and publish images used by Kubernetes Pods",
		"Discover images referenced by selected Pods, save them as OCI Image Layouts, "+
			"inspect the layouts, download them from S3, or publish them to a registry.",
		flags.Example().Arg("save").Arg("dir").Arg("./backup"))
	applyDescription(parser, &options.Image.Save,
		"Capture images referenced by Kubernetes Pods",
		"Discover images used by selected Pods and save their OCI content-addressed data.",
		flags.Example().Arg("dir").Arg("./backup"))
	applyDescription(parser, &options.Image.Push,
		"Publish a saved OCI image layout to a registry",
		"Publish a saved OCI Image Layout to the requested registry prefix.",
		flags.Example().Arg("dir").Arg("./backup").Arg("registry.example.com/recovered"))
	applyDescription(parser, &options.Image.Inspect,
		"Inspect a saved OCI image layout",
		"Show saved image references, manifests, platforms, blob counts, and sizes.",
		flags.Example().Arg("dir").Arg("./backup"))
	applyDescription(parser, &options.Image.Download,
		"Download a saved OCI image layout from S3",
		"Download an OCI Image Layout from S3 into a local directory.",
		flags.Example().Arg("s3").Arg("./backup").Raw("--s3-uri").Raw("s3://bucket/backup"))
	if descriptionErr != nil {
		return fmt.Errorf("configure command descriptions: %w", descriptionErr)
	}

	return nil
}

// setCommandDescription applies a description and structured examples to one registered command
// and fails fast when the command model is inconsistent.
func setCommandDescription(
	parser *flags.Parser,
	target any,
	short, long string,
	examples ...*flags.CommandExample,
) error {
	command, err := parser.CommandFor(target)
	if err != nil {
		return fmt.Errorf("resolve command description target: %w", err)
	}

	command.SetShortDescription(short)
	command.SetLongDescription(long)
	if err := command.SetExamples(examples...); err != nil {
		return fmt.Errorf("set command examples: %w", err)
	}

	return nil
}
