// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	contract "github.com/woozymasta/kube-dump/v2/pkg/profile"
	"go.yaml.in/yaml/v3"
)

// builtinFS contains the profile documents shipped with the binary.
//
//go:embed builtin/*.yaml
var builtinFS embed.FS

// BuiltinInfo describes one profile embedded in the binary.
type BuiltinInfo struct {
	// Name is accepted by profile show and backup --profile.
	Name string
	// Description explains the profile's intended use.
	Description string
}

// builtinDocument couples an embedded filename with the profile metadata it contains.
type builtinDocument struct {
	// metadata identifies the profile independently from its filename.
	metadata contract.Metadata
	// file is the embedded filesystem path used to read the document.
	file string
	// data is the original profile document.
	data []byte
}

// Builtin returns the original YAML document for a built-in profile.
func Builtin(name string) ([]byte, error) {
	name = defaultProfileName(name)
	documents, err := builtinDocuments()
	if err != nil {
		return nil, err
	}

	for _, document := range documents {
		if document.metadata.Name == name {
			return document.data, nil
		}
	}

	return nil, fmt.Errorf("built-in profile %q was not found", name)
}

// BuiltinNames returns aliases declared by embedded profile metadata.
func BuiltinNames() ([]string, error) {
	documents, err := builtinDocuments()
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(documents))
	for _, document := range documents {
		names = append(names, document.metadata.Name)
	}

	sort.Strings(names)
	return names, nil
}

// BuiltinInfos returns embedded profiles and their YAML descriptions.
func BuiltinInfos() ([]BuiltinInfo, error) {
	documents, err := builtinDocuments()
	if err != nil {
		return nil, err
	}

	profiles := make([]BuiltinInfo, 0, len(documents))
	for _, document := range documents {
		profiles = append(profiles, BuiltinInfo{
			Name:        document.metadata.Name,
			Description: document.metadata.Description,
		})
	}

	return profiles, nil
}

// builtinDocuments reads embedded files and derives aliases from metadata.name.
// Filenames remain implementation details of the embedded filesystem.
func builtinDocuments() ([]builtinDocument, error) {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, fmt.Errorf("read embedded profiles: %w", err)
	}

	documents := make([]builtinDocument, 0, len(entries))
	aliases := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isProfileFile(entry.Name()) {
			continue
		}

		file := "builtin/" + entry.Name()
		data, err := builtinFS.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read embedded profile %q: %w", file, err)
		}

		var metadataDocument struct {
			Metadata contract.Metadata `yaml:"metadata"`
		}

		if err := yaml.Unmarshal(data, &metadataDocument); err != nil {
			return nil, fmt.Errorf("decode embedded profile %q: %w", file, err)
		}

		if metadataDocument.Metadata.Name == "" {
			return nil, fmt.Errorf("embedded profile %q has empty metadata.name", file)
		}

		if previous, exists := aliases[metadataDocument.Metadata.Name]; exists {
			return nil, fmt.Errorf(
				"embedded profile alias %q is declared by %q and %q",
				metadataDocument.Metadata.Name, previous, file,
			)
		}

		aliases[metadataDocument.Metadata.Name] = file
		documents = append(documents, builtinDocument{
			file:     file,
			data:     data,
			metadata: metadataDocument.Metadata,
		})
	}

	sort.Slice(documents, func(i, j int) bool {
		return documents[i].metadata.Name < documents[j].metadata.Name
	})

	return documents, nil
}

// defaultProfileName derives a stable display name for a profile source.
func defaultProfileName(source string) string {
	if source == "" {
		return "backup"
	}

	return strings.TrimSuffix(strings.TrimSuffix(source, ".yaml"), ".yml")
}
