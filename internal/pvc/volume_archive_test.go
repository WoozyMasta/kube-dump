// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"archive/tar"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestVolumeAgentRoundTrip(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "data.bin"), []byte{0, 1, 2, 3}, 0o640); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), source, &archive); err != nil {
		t.Fatalf("ExportVolume() error = %v", err)
	}

	destination := t.TempDir()
	if err := ImportVolume(context.Background(), destination, &archive, ExistingEmptyOnly); err != nil {
		t.Fatalf("ImportVolume() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(destination, "nested", "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte{0, 1, 2, 3}) {
		t.Fatalf("restored data = %v", data)
	}
}

func TestValidateVolumeArchiveReportsDecodedStats(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "data"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), source, &archive); err != nil {
		t.Fatal(err)
	}

	stats, err := ValidateVolumeArchive(context.Background(), bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("ValidateVolumeArchive() error = %v", err)
	}
	if stats.SizeBytes != int64(archive.Len()) {
		t.Fatalf("validated size = %d, want %d", stats.SizeBytes, archive.Len())
	}
	if stats.ContentSHA256 == "" {
		t.Fatal("ValidateVolumeArchive() returned an empty digest")
	}
}

func TestValidateVolumeArchiveRejectsTruncatedPayload(t *testing.T) {
	t.Parallel()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "data", Mode: 0o600, Size: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	truncated := archive.Bytes()[:archive.Len()-3]
	if _, err := ValidateVolumeArchive(context.Background(), bytes.NewReader(truncated)); err == nil {
		t.Fatal("ValidateVolumeArchive() accepted a truncated archive")
	}
}

func TestValidateVolumeArchiveRejectsDuplicateAndConflictingPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []tar.Header
	}{
		{
			name: "duplicate",
			entries: []tar.Header{
				{Name: "data", Mode: 0o600},
				{Name: "data", Mode: 0o600},
			},
		},
		{
			name: "file parent",
			entries: []tar.Header{
				{Name: "data", Mode: 0o600},
				{Name: "data/nested", Mode: 0o600},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			for _, header := range test.entries {
				if header.Typeflag == 0 {
					header.Typeflag = tar.TypeReg
				}
				if err := writer.WriteHeader(&header); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateVolumeArchive(context.Background(), bytes.NewReader(archive.Bytes())); err == nil {
				t.Fatal("ValidateVolumeArchive() accepted an invalid path set")
			}
		})
	}
}

func TestVolumeAgentPreservesSafeSymlink(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "target"), []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(source, "link")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}

	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), source, &archive); err != nil {
		t.Fatalf("ExportVolume() error = %v", err)
	}

	destination := t.TempDir()
	if err := ImportVolume(context.Background(), destination, &archive, ExistingEmptyOnly); err != nil {
		t.Fatalf("ImportVolume() error = %v", err)
	}

	linkTarget, err := os.Readlink(filepath.Join(destination, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if linkTarget != "target" {
		t.Fatalf("restored symlink target = %q, want target", linkTarget)
	}
}

func TestVolumeAgentRestoresModesAndMtimes(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	nested := filepath.Join(source, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	fileName := filepath.Join(nested, "data")
	if err := os.WriteFile(fileName, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fileName, 0o600); err != nil {
		t.Fatal(err)
	}
	wantMtime := time.Date(2024, time.February, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(nested, wantMtime, wantMtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fileName, wantMtime, wantMtime); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), source, &archive); err != nil {
		t.Fatalf("ExportVolume() error = %v", err)
	}

	destination := t.TempDir()
	if err := ImportVolume(context.Background(), destination, &archive, ExistingEmptyOnly); err != nil {
		t.Fatalf("ImportVolume() error = %v", err)
	}

	for name, wantMode := range map[string]os.FileMode{
		"nested":      0o700,
		"nested/data": 0o600,
	} {
		info, err := os.Stat(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("stat restored %q: %v", name, err)
		}
		if runtime.GOOS != "windows" {
			if got := info.Mode().Perm(); got != wantMode {
				t.Errorf("restored %q mode = %o, want %o", name, got, wantMode)
			}
		}
		if !info.ModTime().Equal(wantMtime) {
			t.Errorf("restored %q mtime = %s, want %s", name, info.ModTime(), wantMtime)
		}
	}
}

func TestVolumeAgentRejectsHardLinks(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	target := filepath.Join(source, "target")
	link := filepath.Join(source, "link")
	if err := os.WriteFile(target, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, link); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}

	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), source, &archive); err == nil {
		t.Fatal("ExportVolume() accepted hard links")
	} else if !strings.Contains(err.Error(), "hard-link topology is unsupported") {
		t.Fatalf("ExportVolume() error = %v, want hard-link error", err)
	}
}

func TestVolumeAgentPreservesRootMetadata(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	wantMtime := time.Date(2024, time.March, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chmod(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, wantMtime, wantMtime); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), source, &archive); err != nil {
		t.Fatalf("ExportVolume() error = %v", err)
	}

	destination := t.TempDir()
	if err := os.Chmod(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ImportVolume(context.Background(), destination, &archive, ExistingEmptyOnly); err != nil {
		t.Fatalf("ImportVolume() error = %v", err)
	}

	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("restored root mode = %o, want 700", info.Mode().Perm())
	}
	if !info.ModTime().Equal(wantMtime) {
		t.Fatalf("restored root mtime = %s, want %s", info.ModTime(), wantMtime)
	}
}

func TestValidateVolumeArchiveRequiresOneRootMetadataRecord(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		entries []tar.Header
	}{
		{
			name: "empty",
		},
		{
			name: "missing",
			entries: []tar.Header{
				{Name: "data", Mode: 0o600, Typeflag: tar.TypeReg},
			},
		},
		{
			name: "duplicate",
			entries: []tar.Header{
				rootArchiveTestHeader(),
				rootArchiveTestHeader(),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			for _, header := range test.entries {
				if err := writer.WriteHeader(&header); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateVolumeArchive(context.Background(), bytes.NewReader(archive.Bytes())); err == nil {
				t.Fatal("ValidateVolumeArchive() accepted an invalid root metadata sequence")
			}
		})
	}
}

func TestImportVerifiedVolumeRejectsEmptyArchiveBeforeReplacement(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()
	existing := filepath.Join(destination, "existing")
	if err := os.WriteFile(existing, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := tar.NewWriter(&archive).Close(); err != nil {
		t.Fatal(err)
	}
	if err := ImportVerifiedVolume(
		context.Background(), destination, &archive, ExistingReplace,
	); err == nil {
		t.Fatal("ImportVerifiedVolume() accepted an empty archive")
	}

	data, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "preserve" {
		t.Fatalf("existing data = %q, want preserve", data)
	}
}

func rootArchiveTestHeader() tar.Header {
	return tar.Header{
		Name:       rootArchiveName,
		Mode:       0o700,
		Typeflag:   tar.TypeDir,
		PAXRecords: map[string]string{rootMetadataPAXKey: rootMetadataPAXValue},
	}
}

func TestValidateArchiveMetadataRejectsOutOfRangeValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		header tar.Header
	}{
		{name: "uid", header: tar.Header{Name: "data", Uid: -1}},
		{name: "gid", header: tar.Header{Name: "data", Gid: -1}},
		{name: "mtime", header: tar.Header{Name: "data", ModTime: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateArchiveMetadata(&test.header); err == nil {
				t.Fatal("validateArchiveMetadata() accepted an out-of-range header")
			}
		})
	}
}

func TestValidateArchiveHeaderRejectsOutOfRangeSymlinkMode(t *testing.T) {
	t.Parallel()
	header := &tar.Header{
		Name:     "link",
		Typeflag: tar.TypeSymlink,
		Linkname: "target",
		Mode:     int64(^uint32(0)) + 1,
	}

	if _, _, err := validateArchiveHeader(header); err == nil {
		t.Fatal("validateArchiveHeader() accepted an out-of-range symlink mode")
	}
}

func TestArchiveFileModePreservesSpecialPermissionBits(t *testing.T) {
	mode, err := archiveFileMode(0o7755)
	if err != nil {
		t.Fatal(err)
	}

	want := os.FileMode(0o755) | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if mode != want {
		t.Fatalf("archiveFileMode() = %v, want %v", mode, want)
	}
}

func TestVolumeAgentRejectsEscapingSymlink(t *testing.T) {
	if err := validateExportSymlink("nested/link", "../../outside"); err == nil {
		t.Fatal("validateExportSymlink() accepted an escaping target")
	}
	if err := validateImportSymlink(t.TempDir(), "nested/link", "../../outside"); err == nil {
		t.Fatal("validateImportSymlink() accepted an escaping target")
	}
}

func TestImportVolumeRejectsTraversal(t *testing.T) {
	t.Parallel()
	var archive bytes.Buffer
	archive.WriteString("not a tar")
	if err := ImportVolume(context.Background(), t.TempDir(), &archive, ExistingMerge); err == nil {
		t.Fatal("ImportVolume() unexpectedly accepted invalid archive")
	}
	if _, err := safeArchiveName("../escape"); err == nil {
		t.Fatal("safeArchiveName() accepted parent traversal")
	}
	if _, err := safeArchiveName("/absolute"); err == nil {
		t.Fatal("safeArchiveName() accepted absolute path")
	}
	if _, err := safeArchiveName(`nested\file`); err == nil {
		t.Fatal("safeArchiveName() accepted platform separator")
	}
}

func TestImportVolumeValidatesBeforeReplacement(t *testing.T) {
	t.Parallel()
	destination := t.TempDir()
	existing := filepath.Join(destination, "existing")
	if err := os.WriteFile(existing, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "new", Mode: 0o600, Size: 3, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if err := ImportVolume(context.Background(), destination, &archive, ExistingReplace); err == nil {
		t.Fatal("ImportVolume() unexpectedly accepted invalid archive")
	}
	data, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "preserve" {
		t.Fatalf("existing data = %q, want preserve", data)
	}
	if _, err := os.Stat(filepath.Join(destination, "new")); !os.IsNotExist(err) {
		t.Fatalf("new entry was extracted, stat error = %v", err)
	}
}

func TestImportVolumeExistingPolicy(t *testing.T) {
	t.Parallel()
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "existing"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "existing"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), source, &archive); err != nil {
		t.Fatalf("ExportVolume() error = %v", err)
	}
	if err := ImportVolume(context.Background(), destination, &archive, ExistingEmptyOnly); err == nil {
		t.Fatal("empty-only import unexpectedly succeeded")
	}
}

func TestImportVolumeMergeRejectsExistingSocket(t *testing.T) {
	t.Parallel()

	destination := t.TempDir()
	target := filepath.Join(destination, "existing")
	listener, err := net.Listen("unix", target)
	if err != nil {
		t.Skipf("Unix sockets are unavailable: %v", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(target)
	}()

	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "existing"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), source, &archive); err != nil {
		t.Fatalf("ExportVolume() error = %v", err)
	}
	if err := ImportVolume(context.Background(), destination, &archive, ExistingMerge); err == nil {
		t.Fatal("merge import replaced an existing socket")
	}

	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("inspect preserved socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("existing target mode = %v, want socket", info.Mode())
	}
}

func TestImportVolumeReplaceRemovesStaleEntries(t *testing.T) {
	t.Parallel()
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "stale"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(destination, "old-dir"), 0o700); err != nil {
		t.Fatal(err)
	}

	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "current"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), source, &archive); err != nil {
		t.Fatalf("ExportVolume() error = %v", err)
	}
	if err := ImportVolume(context.Background(), destination, &archive, ExistingReplace); err != nil {
		t.Fatalf("ImportVolume() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale entry still exists, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "old-dir")); !os.IsNotExist(err) {
		t.Fatalf("stale directory still exists, stat error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("current data = %q, want new", data)
	}
}

func TestImportVolumeEmptyOnlyAllowsLostFound(t *testing.T) {
	t.Parallel()
	destination := t.TempDir()
	if err := os.Mkdir(filepath.Join(destination, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}

	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "current"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := ExportVolume(context.Background(), source, &archive); err != nil {
		t.Fatalf("ExportVolume() error = %v", err)
	}
	if err := ImportVolume(context.Background(), destination, &archive, ExistingEmptyOnly); err != nil {
		t.Fatalf("ImportVolume() error = %v", err)
	}
}
