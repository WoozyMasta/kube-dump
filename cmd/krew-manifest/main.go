// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// betteralign:ignore

// Command krew-manifest generates a Krew plugin manifest from release archives.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/woozymasta/flags"
	"go.yaml.in/yaml/v3"
)

const (
	archiveOSPattern   = `[a-z][a-z0-9]*`
	archiveArchPattern = `[a-z0-9]+`
)

var releaseArchivePattern = regexp.MustCompile(
	"^kube-dump-(" + archiveOSPattern + ")-(" + archiveArchPattern + `)\.tar\.gz$`,
)

type manifest struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   manifestMeta `yaml:"metadata"`
	Spec       manifestSpec `yaml:"spec"`
}

type manifestMeta struct {
	Name string `yaml:"name"`
}

type manifestSpec struct {
	Version          string             `yaml:"version"`
	Homepage         string             `yaml:"homepage"`
	ShortDescription string             `yaml:"shortDescription"`
	Description      string             `yaml:"description"`
	Caveats          string             `yaml:"caveats"`
	Platforms        []manifestPlatform `yaml:"platforms"`
}

type manifestPlatform struct {
	Selector manifestSelector `yaml:"selector"`
	URI      string           `yaml:"uri"`
	SHA256   string           `yaml:"sha256"`
	Bin      string           `yaml:"bin"`
}

type manifestSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

type releaseArchive struct {
	Name string
	OS   string
	Arch string
}

type commandOptions struct {
	BuildDirectory string `long:"build-dir" description:"Directory containing release archives" default:"build"`
	Version        string `long:"version"   description:"Release version, for example 2.0.0, v2.0.0, or v2.0.0-rc.1" required:"true" validate-regex:"v?[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?"`
	BaseURL        string `long:"base-url"  description:"GitHub release download base URL" default:"https://github.com/woozymasta/kube-dump/releases/download"`
	Output         string `long:"output"    description:"Output path for the generated Krew manifest" required:"true"`
}

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run parses arguments and writes a Krew manifest for the release archives.
func run(args []string, stderr io.Writer) error {
	var command commandOptions
	parser := flags.NewParser(&command, flags.Default&^flags.PrintErrors|flags.StrictPositionalArgs)
	if _, err := parser.ParseArgs(args); err != nil {
		var parserError *flags.Error
		if errors.As(err, &parserError) && parserError.Type == flags.ErrHelp {
			parser.WriteHelp(stderr)
			return nil
		}
		return err
	}

	version := "v" + strings.TrimPrefix(command.Version, "v")
	result, err := buildManifest(command.BuildDirectory, version, strings.TrimRight(command.BaseURL, "/"))
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal Krew manifest: %w", err)
	}

	if err := os.WriteFile(command.Output, data, 0o600); err != nil {
		return fmt.Errorf("write Krew manifest %q: %w", command.Output, err)
	}

	return nil
}

// buildManifest creates a Krew manifest from the platform release archives.
func buildManifest(buildDirectory, version, baseURL string) (manifest, error) {
	archives, err := findReleaseArchives(buildDirectory)
	if err != nil {
		return manifest{}, err
	}

	platforms := make([]manifestPlatform, 0, len(archives))
	archiveRoot := "kube-dump-" + version
	for _, archive := range archives {
		path := filepath.Join(buildDirectory, archive.Name)
		checksum, err := sha256File(path)
		if err != nil {
			return manifest{}, err
		}

		binaryName := "kube-dump"
		if archive.OS == "windows" {
			binaryName += ".exe"
		}

		platforms = append(platforms, manifestPlatform{
			Selector: manifestSelector{MatchLabels: map[string]string{
				"os":   archive.OS,
				"arch": archive.Arch,
			}},
			URI:    baseURL + "/" + version + "/" + archive.Name,
			SHA256: checksum,
			Bin:    archiveRoot + "/" + binaryName,
		})
	}

	return manifest{
		APIVersion: "krew.googlecontainertools.github.com/v1alpha2",
		Kind:       "Plugin",
		Metadata: manifestMeta{
			Name: "kube-dump",
		},
		Spec: manifestSpec{
			Version:          version,
			Homepage:         "https://github.com/woozymasta/kube-dump",
			ShortDescription: "Back up and export Kubernetes data",
			Description: "kube-dump is a standalone Kubernetes backup and export utility.\n" +
				"It captures Kubernetes resources, PVC filesystems, and container images " +
				"and stores them in local directories, Git repositories, S3-compatible storage, or archive files.\n" +
				"It supports profiles, selectors, and optional encryption for sensitive data.",
			Caveats: "kube-dump accesses the Kubernetes API directly " +
				"and requires a kubeconfig or in-cluster credentials with permissions for the selected data.\n" +
				"PVC backups may additionally require CSI snapshot support or permission " +
				"to create temporary helper resources in the target namespaces.",
			Platforms: platforms,
		},
	}, nil
}

// findReleaseArchives returns release archives recognized by the Krew platform pattern.
func findReleaseArchives(buildDirectory string) ([]releaseArchive, error) {
	entries, err := os.ReadDir(buildDirectory)
	if err != nil {
		return nil, fmt.Errorf("read build directory %q: %w", buildDirectory, err)
	}

	archives := make([]releaseArchive, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		matches := releaseArchivePattern.FindStringSubmatch(entry.Name())
		if matches == nil {
			continue
		}

		archives = append(archives, releaseArchive{
			Name: entry.Name(),
			OS:   matches[1],
			Arch: matches[2],
		})
	}

	if len(archives) == 0 {
		return nil, fmt.Errorf(
			"no Krew release archives matching kube-dump-<os>-<arch>.tar.gz found in %q",
			buildDirectory,
		)
	}

	sort.Slice(archives, func(i, j int) bool {
		return archives[i].Name < archives[j].Name
	})

	return archives, nil
}

// sha256File calculates the hexadecimal SHA-256 checksum of a release archive.
func sha256File(path string) (checksum string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open release archive %q: %w", path, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			closeErr = fmt.Errorf("close release archive %q: %w", path, closeErr)
			if err == nil {
				err = closeErr
				return
			}

			err = errors.Join(err, closeErr)
		}
	}()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash release archive %q: %w", path, err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}
