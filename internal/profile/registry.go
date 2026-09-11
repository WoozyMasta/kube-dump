// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	contract "github.com/woozymasta/kube-dump/v2/pkg/profile"
	"go.yaml.in/yaml/v3"
)

// SourceKind identifies where a profile is stored.
type SourceKind string

const (
	// BuiltinSource identifies a profile embedded in the executable.
	BuiltinSource SourceKind = "builtin"
	// GlobalSource identifies a profile stored in the user profile directory.
	GlobalSource SourceKind = "global"
)

// CatalogEntry describes a profile for catalog and source-reporting commands.
type CatalogEntry struct {
	// Name is the alias accepted by profile commands.
	Name string
	// Description is read from metadata when it is available.
	Description string
	// Source identifies whether the profile is built-in or global.
	Source SourceKind
	// Path is the filesystem path for a global profile and is empty for built-ins.
	Path string
}

// Registry resolves built-in and user-installed profiles.
type Registry struct {
	// directory is the global profile directory.
	directory string
}

// NewRegistry creates a profile registry using an explicit directory or the
// platform configuration directory when directory is empty.
func NewRegistry(directory string) (*Registry, error) {
	if directory == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("resolve user config directory: %w", err)
		}

		directory = filepath.Join(configDir, "kube-dump", "profiles")
	}

	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve profile directory: %w", err)
	}

	return &Registry{directory: filepath.Clean(directory)}, nil
}

// Directory returns the absolute directory used for global profiles.
func (r *Registry) Directory() string {
	if r == nil {
		return ""
	}

	return r.directory
}

// List returns built-in and global aliases without fully validating documents.
func (r *Registry) List() ([]CatalogEntry, error) {
	if r == nil {
		return nil, errors.New("profile registry is nil")
	}

	entries, err := BuiltinInfos()
	if err != nil {
		return nil, err
	}

	result := make([]CatalogEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, CatalogEntry{
			Name:        entry.Name,
			Description: entry.Description,
			Source:      BuiltinSource,
		})
	}

	global, err := os.ReadDir(r.directory)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read global profile directory: %w", err)
	}

	for _, entry := range global {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !isProfileFile(entry.Name()) {
			continue
		}

		path := filepath.Join(r.directory, entry.Name())
		metadata, ok := readMetadata(path)
		if !ok || metadata.Name == "" {
			continue
		}

		result = append(result, CatalogEntry{
			Name:        metadata.Name,
			Description: metadata.Description,
			Source:      GlobalSource,
			Path:        path,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}

		return result[i].Source < result[j].Source
	})

	return result, nil
}

// Resolve reads a built-in alias, global alias, qualified alias, or file path.
func (r *Registry) Resolve(source string) ([]byte, error) {
	if r == nil {
		return nil, errors.New("profile registry is nil")
	}

	if name, ok := qualifiedName(source, "builtin:"); ok {
		return Builtin(name)
	}
	if name, ok := qualifiedName(source, "global:"); ok {
		return r.readGlobal(name)
	}
	if name, ok := strings.CutPrefix(source, "kube-dump://"); ok {
		return Builtin(strings.TrimSuffix(name, ".yaml"))
	}
	if data, err := os.ReadFile(source); err == nil {
		return data, nil
	}

	builtin, builtinErr := Builtin(source)
	global, globalErr := r.readGlobal(source)
	if builtinErr == nil && globalErr == nil {
		return nil, fmt.Errorf("profile %q is ambiguous; use builtin:%s or global:%s", source, source, source)
	}
	if builtinErr == nil {
		return builtin, nil
	}
	if globalErr == nil {
		return global, nil
	}

	return nil, fmt.Errorf("resolve profile %q: built-in: %v; global: %v", source, builtinErr, globalErr)
}

// Which returns the canonical source identifier for a profile.
func (r *Registry) Which(source string) (string, error) {
	if r == nil {
		return "", errors.New("profile registry is nil")
	}

	if name, ok := qualifiedName(source, "builtin:"); ok {
		if _, err := Builtin(name); err != nil {
			return "", err
		}

		return "kube-dump://" + strings.TrimSuffix(name, ".yaml") + ".yaml", nil
	}

	if name, ok := qualifiedName(source, "global:"); ok {
		path, err := r.globalPath(name)
		if err != nil {
			return "", err
		}

		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("profile %q was not found: %w", source, err)
		}

		return filepath.Abs(path)
	}

	if name := strings.TrimSuffix(strings.TrimPrefix(source, "kube-dump://"), ".yaml"); name != source {
		if _, err := Builtin(name); err != nil {
			return "", err
		}
		return "kube-dump://" + name + ".yaml", nil
	}

	if data, err := os.Stat(source); err == nil && !data.IsDir() {
		path, err := filepath.Abs(source)
		if err != nil {
			return "", err
		}

		return path, nil
	}

	entries, err := r.List()
	if err != nil {
		return "", err
	}

	var matches []CatalogEntry
	for _, entry := range entries {
		if entry.Name == source {
			matches = append(matches, entry)
		}
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("profile %q was not found", source)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("profile %q is ambiguous; use builtin:%s or global:%s", source, source, source)
	}
	if matches[0].Source == BuiltinSource {
		return "kube-dump://" + source + ".yaml", nil
	}

	return matches[0].Path, nil
}

// Install validates a profile and stores it under metadata.name in the global directory.
func (r *Registry) Install(source string, force bool) (string, error) {
	data, err := r.Resolve(source)
	if err != nil {
		return "", err
	}

	compiled, err := LoadBytes(data)
	if err != nil {
		return "", fmt.Errorf("validate profile before install: %w", err)
	}

	path, err := r.globalPath(compiled.Name())
	if err != nil {
		return "", err
	}

	if !force {
		if _, err := os.Stat(path); err == nil {
			return "", fmt.Errorf("profile %q already exists; use --force to replace it", compiled.Name())
		}
	}

	if err := os.MkdirAll(r.directory, 0o750); err != nil {
		return "", fmt.Errorf("create global profile directory: %w", err)
	}
	if err := atomicWrite(path, data, force); err != nil {
		return "", fmt.Errorf("write installed profile: %w", err)
	}

	return path, nil
}

// Edit opens a global profile in the configured editor
// and validates the resulting document after the editor exits successfully.
func (r *Registry) Edit(ctx context.Context, name string) error {
	if r == nil {
		return errors.New("profile registry is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	path, err := r.globalPath(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("global profile %q is not installed: %w", name, err)
	}

	command := os.Getenv("KUBE_DUMP_EDITOR")
	if command == "" {
		command = os.Getenv("VISUAL")
	}
	if command == "" {
		command = os.Getenv("EDITOR")
	}
	if command == "" {
		return errors.New("set KUBE_DUMP_EDITOR, VISUAL, or EDITOR to edit profiles")
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		return errors.New("configured profile editor is empty")
	}

	arguments := make([]string, 0, len(parts))
	arguments = append(arguments, parts[1:]...)
	arguments = append(arguments, path)

	//nolint:gosec // The editor is explicitly selected by the user environment.
	editor := exec.CommandContext(ctx, parts[0], arguments...)
	editor.Stdin = os.Stdin
	editor.Stdout = os.Stdout
	editor.Stderr = os.Stderr

	if err := editor.Run(); err != nil {
		return fmt.Errorf("run profile editor: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read edited profile: %w", err)
	}
	if _, err := LoadBytes(data); err != nil {
		return fmt.Errorf("validate edited profile: %w", err)
	}

	return nil
}

// Copy copies any resolvable and valid profile into the global catalog.
func (r *Registry) Copy(source, name string, force bool) (string, error) {
	if r == nil {
		return "", errors.New("profile registry is nil")
	}

	data, err := r.Resolve(source)
	if err != nil {
		return "", err
	}
	if _, err := LoadBytes(data); err != nil {
		return "", fmt.Errorf("validate profile before copy: %w", err)
	}
	if name == "" {
		return "", errors.New("global profile name is required")
	}

	var document contract.Profile
	if err := yaml.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("decode profile before copy: %w", err)
	}

	document.Metadata.Name = name
	data, err = yaml.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode copied profile: %w", err)
	}

	path, err := r.globalPath(name)
	if err != nil {
		return "", err
	}

	if !force {
		if _, err := os.Stat(path); err == nil {
			return "", fmt.Errorf("profile %q already exists; use --force to replace it", name)
		}
	}

	if err := os.MkdirAll(r.directory, 0o750); err != nil {
		return "", fmt.Errorf("create global profile directory: %w", err)
	}
	if err := atomicWrite(path, data, force); err != nil {
		return "", fmt.Errorf("write copied profile: %w", err)
	}

	return path, nil
}

// Remove deletes a global profile by alias and never removes built-in files.
func (r *Registry) Remove(name string) error {
	path, err := r.globalPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove global profile %q: %w", name, err)
	}

	return nil
}

// readGlobal reads one global profile alias and rejects ambiguous aliases.
func (r *Registry) readGlobal(name string) ([]byte, error) {
	path, err := r.globalPath(name)
	if err != nil {
		return nil, err
	}

	return os.ReadFile(path)
}

// globalPath resolves a metadata.name alias to its YAML file.
func (r *Registry) globalPath(name string) (string, error) {
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
	if name == "" || filepath.Base(name) != name {
		return "", fmt.Errorf("invalid global profile name %q", name)
	}

	entries, err := os.ReadDir(r.directory)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read global profile directory: %w", err)
	}

	var matches []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !isProfileFile(entry.Name()) {
			continue
		}

		path := filepath.Join(r.directory, entry.Name())
		metadata, ok := readMetadata(path)
		if ok && metadata.Name == name {
			matches = append(matches, path)
		}
	}

	if len(matches) > 1 {
		return "", fmt.Errorf("global profile alias %q is defined by multiple files", name)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}

	return filepath.Join(r.directory, name+".yaml"), nil
}

// readMetadata reads only profile metadata and deliberately skips full validation.
func readMetadata(path string) (contract.Metadata, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return contract.Metadata{}, false
	}

	var document struct {
		Metadata contract.Metadata `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return contract.Metadata{}, false
	}

	return document.Metadata, true
}

// qualifiedName strips a source qualifier and reports whether it was present.
func qualifiedName(source, prefix string) (string, bool) {
	if !strings.HasPrefix(source, prefix) {
		return "", false
	}

	return strings.TrimPrefix(source, prefix), true
}

// isProfileFile reports whether a directory entry is a supported profile file.
func isProfileFile(name string) bool {
	extension := strings.ToLower(filepath.Ext(name))
	return extension == ".yaml" || extension == ".yml"
}

// atomicWrite replaces a profile using temporary files in the destination directory
// so a failed write cannot leave a truncated profile behind.
func atomicWrite(path string, data []byte, replace bool) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".profile-*")
	if err != nil {
		return err
	}

	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}

	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}

	backupPath, err := moveExistingProfile(path, replace)
	if err != nil {
		return err
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		if backupPath != "" {
			_ = os.Rename(backupPath, path)
		}
		return err
	}
	if backupPath != "" {
		_ = os.Remove(backupPath)
	}

	return nil
}

// moveExistingProfile moves an existing regular file aside for an atomic replacement.
func moveExistingProfile(path string, replace bool) (string, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("destination %q is not a regular file", path)
	}
	if !replace {
		return "", fmt.Errorf("destination %q already exists", path)
	}

	backup, err := os.CreateTemp(filepath.Dir(path), ".profile-backup-*")
	if err != nil {
		return "", err
	}

	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupPath)
		return "", err
	}
	if err := os.Remove(backupPath); err != nil {
		return "", err
	}
	if err := os.Rename(path, backupPath); err != nil {
		return "", err
	}

	return backupPath, nil
}
