package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/woozymasta/flags"
	"github.com/woozymasta/kube-dump/v2/internal/archive"
	"github.com/woozymasta/kube-dump/v2/internal/compress"
	agecrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/age"
	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/crypto/keyring"
	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"github.com/woozymasta/kube-dump/v2/pkg/profile"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestProfileCommandsUseCurrentNames(t *testing.T) {
	var output bytes.Buffer
	command := ProfileShowCommand{commandStreams: commandStreams{output: &output}}
	command.Positional.Name = "raw"
	if err := command.Execute(nil); err != nil {
		t.Fatalf("ProfileShowCommand.Execute() error = %v", err)
	}
	if !bytes.Contains(output.Bytes(), []byte("name: raw")) {
		t.Fatalf("profile output does not contain the requested profile: %q", output.String())
	}
}

func TestProfileSchemaCommandPrintsJSON(t *testing.T) {
	var output bytes.Buffer
	command := ProfileSchemaCommand{commandStreams: commandStreams{output: &output}}
	if err := command.Execute(nil); err != nil {
		t.Fatalf("ProfileSchemaCommand.Execute() error = %v", err)
	}
	if !json.Valid(output.Bytes()) {
		t.Fatalf("profile schema is not valid JSON: %q", output.String())
	}
}

func TestImageSelectionNamespacesBroadensForPatterns(t *testing.T) {
	selection := profile.ImageSelectionSpec{
		ObjectScope: profile.ObjectScope{
			Namespaces: []string{"production-*"},
		},
	}

	if namespaces := imageSelectionNamespaces(selection); namespaces != nil {
		t.Fatalf("namespace pattern was narrowed into an API scope: %#v", namespaces)
	}
}

func TestParserAcceptsRewriteCommandTree(t *testing.T) {
	commands := [][]string{
		{"resource", "save", "dir", "./backup"},
		{"resource", "save", "git", "./backup"},
		{"resource", "save", "archive", "./backup.tar.zst"},
		{"resource", "save", "archive-s3", "--s3-uri", "s3://bucket/backup.tar.zst"},
		{"pvc", "save", "s3", "--s3-uri", "s3://bucket/data"},
		{"pvc", "download", "s3", "./pvc-data", "--s3-uri", "s3://bucket/data"},
		{"image", "save", "dir", "./backup"},
		{"image", "save", "s3", "--s3-uri", "s3://bucket"},
		{"resource", "cat", "dir", "./backup"},
		{"resource", "cat", "archive", "./backup.tar.zst"},
		{"resource", "cat", "archive-s3", "--s3-uri", "s3://bucket/backup.tar.zst"},
		{"image", "push", "dir", "./backup", "registry.example.com/recovered"},
		{"image", "push", "dir", "./backup", "registry.example.com/recovered", "ghcr.io/acme/api:v1"},
		{"image", "push", "s3", "registry.example.com/recovered", "--s3-uri", "s3://bucket"},
		{"image", "push", "s3", "registry.example.com/recovered", "ghcr.io/acme/api:v1", "--s3-uri", "s3://bucket"},
		{"resource", "inspect", "dir", "./backup"},
		{"resource", "inspect", "git", "./backup"},
		{"resource", "inspect", "s3", "--s3-uri", "s3://bucket/backup"},
		{"resource", "inspect", "archive", "./backup.tar.zst"},
		{"resource", "inspect", "archive-s3", "--s3-uri", "s3://bucket/backup.tar.zst"},
		{"resource", "download", "archive", "./backup.tar.zst", "./copy.tar.zst"},
		{"resource", "encrypt", "dir", "./plain", "./encrypted"},
		{"resource", "decrypt", "archive", "./backup.tar.zst.age", "./backup.tar.zst"},
		{"resource", "extract", "archive", "./backup.tar.zst", "./extracted"},
		{"pvc", "download", "s3", "./pvc-data", "--s3-uri", "s3://bucket/pvc"},
		{"pvc", "encrypt", "backup.tar.zst", "backup.tar.zst.age"},
		{"pvc", "decrypt", "backup.tar.zst.age", "backup.tar.zst"},
		{"pvc", "extract", "backup.tar.zst", "./pvc"},
		{"pvc", "restore", "dir", "./pvc-data", "--pvc", "production/postgres-data", "--target-pvc", "recovery/postgres-data"},
		{"pvc", "restore", "s3", "--s3-uri", "s3://bucket/pvc", "--pvc", "production/postgres-data", "--target-pvc", "recovery/postgres-data"},
		{"pvc", "inspect", "dir", "./pvc-data"},
		{"pvc", "inspect", "s3", "--s3-uri", "s3://bucket/pvc/"},
		{"image", "inspect", "dir", "./backup"},
		{"image", "inspect", "s3", "--s3-uri", "s3://bucket"},
		{"image", "download", "s3", "./backup", "--s3-uri", "s3://bucket"},
		{"profile", "show", "backup"},
		{"profile", "schema"},
		{"key", "generate"},
		{"key", "rotate", "./backup"},
		{"key", "rewrap", "./backup"},
	}

	for _, args := range commands {
		t.Run(args[0], func(t *testing.T) {
			options := Options{}
			parser, err := newParser(&options)
			if err != nil {
				t.Fatalf("newParser() error = %v", err)
			}
			parser.CommandHandler = func(flags.Commander, []string) error { return nil }
			if _, err := parser.ParseArgs(args); err != nil {
				t.Fatalf("ParseArgs(%q) error = %v", args, err)
			}
		})
	}
}

func TestParserReadsRegistryMirrorMap(t *testing.T) {
	options := Options{}
	parser, err := newParser(&options)
	if err != nil {
		t.Fatalf("newParser() error = %v", err)
	}
	parser.CommandHandler = func(flags.Commander, []string) error { return nil }

	if _, err := parser.ParseArgs([]string{
		"image", "save", "dir", "./backup",
		"--registry-mirror", "docker.io=mirror.example.com:5000",
	}); err != nil {
		t.Fatalf("ParseArgs() error = %v", err)
	}

	if got := options.Image.Save.RegistryMirrors["docker.io"]; got != "mirror.example.com:5000" {
		t.Fatalf("registry mirror = %q, want %q", got, "mirror.example.com:5000")
	}
}

func TestParserReadsS3URIFromEnvironment(t *testing.T) {
	t.Setenv("KUBE_DUMP_S3_URI", "s3://bucket/from-env")

	options := Options{}
	parser, err := newParser(&options)
	if err != nil {
		t.Fatalf("newParser() error = %v", err)
	}
	parser.CommandHandler = func(flags.Commander, []string) error { return nil }

	if _, err := parser.ParseArgs([]string{"image", "inspect", "s3"}); err != nil {
		t.Fatalf("ParseArgs() error = %v", err)
	}
	if got := options.Image.Inspect.S3.URI; got != "s3://bucket/from-env" {
		t.Fatalf("S3 URI = %q, want %q", got, "s3://bucket/from-env")
	}
}

func TestKeyGenerateWritesParseableIdentity(t *testing.T) {
	var output bytes.Buffer
	command := KeyGenerateCommand{commandStreams: commandStreams{output: &output}}
	if err := command.Execute(nil); err != nil {
		t.Fatalf("KeyGenerateCommand.Execute() error = %v", err)
	}

	identities, err := age.ParseIdentities(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatalf("generated identity is not parseable: %v", err)
	}
	if len(identities) != 1 {
		t.Fatalf("generated identity count = %d, want 1", len(identities))
	}
	if !bytes.Contains(output.Bytes(), []byte("# public key: age1")) {
		t.Fatalf("generated identity does not contain its public recipient: %q", output.String())
	}
}

func TestKeyGenerateDoesNotOverwriteIdentityFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.txt")
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := KeyGenerateCommand{Output: path}
	if err := command.Execute(nil); err == nil {
		t.Fatal("KeyGenerateCommand.Execute() overwrote an existing identity file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing\n" {
		t.Fatalf("existing identity file changed: %q", data)
	}
}

func TestKeyringMaintenanceCommandsRotateAndRewrap(t *testing.T) {
	first, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	second, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := keyring.Open(root, []age.Recipient{first.Recipient()}, nil); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(root, "identity.txt")
	if err := os.WriteFile(identityPath, []byte(first.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var rotate KeyRotateCommand
	rotate.Positional.Directory = root
	rotate.IdentityFiles = []string{identityPath}
	rotate.Recipients = []string{first.Recipient().String()}
	if err := rotate.Execute(nil); err != nil {
		t.Fatalf("KeyRotateCommand.Execute() error = %v", err)
	}

	var rewrap KeyRewrapCommand
	rewrap.Positional.Directory = root
	rewrap.IdentityFiles = []string{identityPath}
	rewrap.Recipients = []string{second.Recipient().String()}
	if err := rewrap.Execute(nil); err != nil {
		t.Fatalf("KeyRewrapCommand.Execute() error = %v", err)
	}
	if _, err := keyring.OpenExisting(root, []age.Identity{second}); err != nil {
		t.Fatalf("OpenExisting() after CLI maintenance error = %v", err)
	}
}

func TestRunRequiresContext(t *testing.T) {
	//lint:ignore SA1012 this test intentionally verifies the nil-context guard.
	if err := Run(nil, nil, nil, nil); err == nil {
		t.Fatal("Run() accepted a nil context")
	}

	if err := Run(
		context.Background(),
		[]string{"profile", "ls"},
		&bytes.Buffer{}, &bytes.Buffer{},
	); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestValidateParsedOptionsRequiresIdentityForAES256SIV(t *testing.T) {
	opts := &Options{}
	opts.Resource.Save.FieldEncryption = fieldcrypto.AES256SIV

	err := validateParsedOptions(opts)
	if err == nil || err.Error() != "--field-encryption=aes-siv requires --identity=PATH" {
		t.Fatalf("validateParsedOptions() error = %v", err)
	}

	opts.Resource.Save.IdentityFiles = []string{"identity.txt"}
	if err := validateParsedOptions(opts); err != nil {
		t.Fatalf("validateParsedOptions() rejected identity: %v", err)
	}
}

func TestResourcesArchivePreflightsArchiveRecipients(t *testing.T) {
	command := ResourcesArchiveCommand{
		ArchiveDestinationPath: ArchiveDestinationPath{Path: filepath.Join(t.TempDir(), "backup.tar.zst.age")},
		ResourceArchiveOptions: ResourceArchiveOptions{
			Format:                TarZSTDAgeFormat,
			ArchiveRecipientsURLs: []string{"ftp://keys.example.test/recipients"},
		},
	}

	err := command.Execute(nil)
	if err == nil || !strings.Contains(err.Error(), "prepare archive encryption") {
		t.Fatalf("archive preflight error = %v", err)
	}
}

func TestParserRejectsAES256SIVWithoutIdentity(t *testing.T) {
	options := Options{}
	parser, err := newParser(&options)
	if err != nil {
		t.Fatalf("newParser() error = %v", err)
	}

	_, err = parser.ParseArgs([]string{
		"resource", "save", "dir", "./backup", "--field-encryption=aes-siv",
	})
	if err == nil || err.Error() != "--field-encryption=aes-siv requires --identity=PATH" {
		t.Fatalf("ParseArgs() error = %v", err)
	}
}

func TestParserRejectsIdentityPassphraseSourceConflict(t *testing.T) {
	options := Options{}
	parser, err := newParser(&options)
	if err != nil {
		t.Fatalf("newParser() error = %v", err)
	}

	_, err = parser.ParseArgs([]string{
		"resource", "cat", "dir", "./backup",
		"--identity-passphrase=one",
		"--identity-passphrase-file=passphrase.txt",
	})
	if err == nil {
		t.Fatal("ParseArgs() accepted conflicting identity passphrase sources")
	}
}

func TestSimplifyCommandErrorCollapsesIdentityPassphraseContext(t *testing.T) {
	wrapped := fmt.Errorf("prepare resource backup: %w", fmt.Errorf(
		"configure field encryption: %w", agecrypto.ErrIdentityPassphraseRequired))

	if got := simplifyCommandError(wrapped); got != agecrypto.ErrIdentityPassphraseRequired {
		t.Fatalf("simplifyCommandError() = %v, want %v", got, agecrypto.ErrIdentityPassphraseRequired)
	}
}

func TestResourceDirectorySinkWritesReadableYAML(t *testing.T) {
	root := t.TempDir()
	sink, err := export.NewDirectorySink(root)
	if err != nil {
		t.Fatal(err)
	}
	object, err := state.NewObject(state.Identity{
		Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment",
		Namespace: "status", Name: "api",
	}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "api", "namespace": "status",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	changed, err := sink.Write(context.Background(), object)
	if err != nil || !changed {
		t.Fatalf("Write() = changed %t, error %v", changed, err)
	}

	target := filepath.Join(root, "apps", "v1", "deployments", "status", "api.yaml")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("apiVersion: apps/v1")) {
		t.Fatalf("resource file is not readable Kubernetes YAML: %q", data)
	}

	changed, err = sink.Write(context.Background(), object)
	if err != nil || changed {
		t.Fatalf("second Write() = changed %t, error %v", changed, err)
	}
}

func TestInspectPvcDirectoryIgnoresResourceFiles(t *testing.T) {
	root := t.TempDir()
	pvcPath := filepath.Join(root, "volumes", "status", "data", "20260902T120000Z", "data.tar.zst")
	if err := os.MkdirAll(filepath.Dir(pvcPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pvcPath, []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	resourcePath := filepath.Join(root, "apps", "v1", "deployments", "status", "api.yaml")
	if err := os.MkdirAll(filepath.Dir(resourcePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resourcePath, []byte("apiVersion: apps/v1\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	command := InspectPvcDirCommand{
		SourcePath:     SourcePath{Path: root},
		commandStreams: commandStreams{output: &output},
	}
	if err := command.Execute(nil); err != nil {
		t.Fatalf("InspectPvcDirCommand.Execute() error = %v", err)
	}
	if !bytes.Contains(output.Bytes(), []byte("PVC artifacts: 1")) {
		t.Fatalf("PVC report does not contain one artifact: %q", output.String())
	}
	if bytes.Contains(output.Bytes(), []byte("Objects:       1")) {
		t.Fatalf("PVC report counted a resource object: %q", output.String())
	}
}

func TestRenderDirectoryWritesStableYAML(t *testing.T) {
	root := t.TempDir()
	sink, err := export.NewDirectorySink(resourceRoot(root))
	if err != nil {
		t.Fatal(err)
	}

	object, err := state.NewObject(state.Identity{
		Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment",
		Namespace: "status", Name: "api",
	}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "status"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(context.Background(), object); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	command := ResourceCatDirCommand{
		SourcePath:     SourcePath{Path: root},
		commandStreams: commandStreams{output: &output},
	}
	if err := command.Execute(nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("kind: Deployment")) {
		t.Fatalf("cat output does not contain the object: %q", output.String())
	}
}

func TestInspectDirectoryOmitsArchiveMetadata(t *testing.T) {
	root := t.TempDir()
	sink, err := export.NewDirectorySink(resourceRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	object, err := state.NewObject(state.Identity{
		Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment",
		Namespace: "status", Name: "api",
	}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "status"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(context.Background(), object); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	command := InspectDirCommand{
		SourcePath:     SourcePath{Path: root},
		commandStreams: commandStreams{output: &output},
	}
	if err := command.Execute(nil); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("Created:")) ||
		bytes.Contains(output.Bytes(), []byte("Compression:")) {
		t.Fatalf("directory report contains archive metadata: %q", output.String())
	}
	if !bytes.Contains(output.Bytes(), []byte("Objects:       1")) {
		t.Fatalf("directory report misses object count: %q", output.String())
	}
}

func TestArchiveCommandsRoundTripValidArchive(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	input := filepath.Join(root, "input.tar.zst")
	encrypted := filepath.Join(root, "input.tar.zst.age")
	decrypted := filepath.Join(root, "decrypted.tar.zst")
	archiveData, err := testArchiveData()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, archiveData, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "identity.txt"),
		[]byte(identity.String()+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	encrypt := EncryptArchiveCommand{
		Positional: struct {
			Input  string `required:"true" positional-arg-name:"INPUT"  description:"Input archive path"`
			Output string `required:"true" positional-arg-name:"OUTPUT" description:"Encrypted archive path"`
		}{Input: input, Output: encrypted},
		ArchiveOptions: ArchiveOptions{Recipients: []string{identity.Recipient().String()}},
	}
	if err := encrypt.Execute(nil); err != nil {
		t.Fatal(err)
	}

	decrypt := DecryptArchiveCommand{
		Positional: struct {
			Input  string `required:"true" positional-arg-name:"INPUT"  description:"Encrypted archive path"`
			Output string `required:"true" positional-arg-name:"OUTPUT" description:"Decrypted archive path"`
		}{Input: encrypted, Output: decrypted},
		IdentityOptions: IdentityOptions{
			IdentityFiles: []string{filepath.Join(root, "identity.txt")},
		},
	}
	if err := decrypt.Execute(nil); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(decrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, archiveData) {
		t.Fatal("archive encryption round-trip changed the archive bytes")
	}

	if report, err := archive.Inspect(bytes.NewReader(data), compress.Zstandard); err != nil {
		t.Fatalf("inspect decrypted archive: %v", err)
	} else if report.Objects != 1 {
		t.Fatalf("decrypted archive object count = %d, want 1", report.Objects)
	}
}

// testArchiveData creates the smallest valid kube-dump archive
// used by the standalone archive encryption test.
func testArchiveData() ([]byte, error) {
	const manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fixture\n"

	var output bytes.Buffer
	writer, err := archive.NewBackupWriter(
		&output,
		archive.Config{Compression: compress.Config{Algorithm: compress.Zstandard}},
	)
	if err != nil {
		return nil, err
	}

	if err := writer.Add(archive.Entry{
		Path:   "resources/core/v1/configmaps/default/fixture.yaml",
		Mode:   0o640,
		Size:   int64(len(manifest)),
		Reader: bytes.NewReader([]byte(manifest)),
	}); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	return output.Bytes(), nil
}

func TestWriteBackupArchiveUsesCanonicalExportTree(t *testing.T) {
	root := t.TempDir()
	resource := filepath.Join(root, "resources", "apps", "v1", "deployments", "status")
	if err := os.MkdirAll(resource, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(resource, "api.yaml"),
		[]byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n  namespace: status\n"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	output := filepath.Join(t.TempDir(), "backup.tar.zst")
	if err := writeBackupArchive(context.Background(), root, output, TarZSTDFormat, nil, nil); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	metadata, entries, err := archive.Verify(file, compress.Zstandard)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Compression != compress.Zstandard || entries != 6 {
		t.Fatalf("archive metadata = %#v, entries = %d", metadata, entries)
	}
}

func TestRenderArchiveReadsEncryptedArchive(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	resource := filepath.Join(root, "resources", "apps", "v1", "deployments", "status")
	if err := os.MkdirAll(resource, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(resource, "api.yaml"),
		[]byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n  namespace: status\n"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz.age")
	recipients, err := prepareArchiveRecipients(
		TarGZIPAgeFormat,
		[]string{identity.Recipient().String()},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBackupArchive(
		context.Background(),
		root,
		archivePath,
		TarGZIPAgeFormat,
		recipients,
		nil,
	); err != nil {
		t.Fatal(err)
	}

	identityPath := filepath.Join(t.TempDir(), "identity.txt")
	if err := os.WriteFile(identityPath, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	command := ResourceCatArchiveCommand{
		ArchiveSourcePath: ArchiveSourcePath{Path: archivePath},
		IdentityOptions: IdentityOptions{
			IdentityFiles: []string{identityPath},
		},
		commandStreams: commandStreams{output: &output},
	}
	if err := command.Execute(nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("kind: Deployment")) {
		t.Fatalf("cat output does not contain the decrypted object: %q", output.String())
	}
}
