// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Command update-version updates release-version references in repository files.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/woozymasta/flags"
)

var (
	versionPattern = regexp.MustCompile(`v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?`)
	refPattern     = regexp.MustCompile(`\?ref=v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?`)
	imagePattern   = regexp.MustCompile(`kube-dump:[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?`)
	modulePattern  = regexp.MustCompile(`kube-dump@v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?`)
	releasePattern = regexp.MustCompile(`releases/download/v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?/kube-dump`)
	newTagPattern  = regexp.MustCompile(`newTag:([ \t]+(&?[A-Za-z_][A-Za-z0-9_-]*[ \t]+)?)[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?`)
)

type options struct {
	root    string
	version string
	include []string
	check   bool
}

type commandOptions struct {
	Root    string   `long:"root"    description:"Repository root" default:"."`
	Version string   `long:"version" description:"Release version, for example 2.0.0, v2.0.0, or v2.0.0-rc.1" required:"true" validate-regex:"v?[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?"`
	Include []string `long:"include" description:"File basename or repository-relative path mask; repeatable"`
	Check   bool     `long:"check"   description:"Check references without modifying files"`
}

type result struct {
	files    []fileResult
	warnings []warning
}

type fileResult struct {
	path         string
	replacements int
}

type warning struct {
	path    string
	version string
	line    int
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run parses command-line arguments and updates managed version references.
func run(args []string, stdout, stderr io.Writer) error {
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
	updated, err := update(options{
		root:    command.Root,
		version: version,
		include: command.Include,
		check:   command.Check,
	})

	for _, file := range updated.files {
		action := "updated"
		if command.Check {
			action = "would update"
		}
		if _, err := fmt.Fprintf(stdout, "%s %s: %d replacement(s)\n", action, file.path, file.replacements); err != nil {
			return fmt.Errorf("write update result: %w", err)
		}
	}

	for _, item := range updated.warnings {
		if _, err := fmt.Fprintf(stderr, "WARN %s:%d: version %s may require manual review\n", item.path, item.line, item.version); err != nil {
			return fmt.Errorf("write version warning: %w", err)
		}
	}
	if err != nil {
		return err
	}

	return nil
}

// update scans included files, replaces managed references, and reports stale versions.
func update(options options) (result, error) {
	version := options.version
	imageVersion := strings.TrimPrefix(version, "v")

	root := options.root
	if root == "" {
		root = "."
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return result{}, fmt.Errorf("resolve repository root: %w", err)
	}

	include := options.include
	if len(include) == 0 {
		include = []string{"*.md", "kustomization.yaml"}
	}

	var output result
	warningSeen := make(map[string]struct{})
	var stale int
	rootDir, err := os.OpenRoot(root)
	if err != nil {
		return result{}, fmt.Errorf("open repository root: %w", err)
	}
	defer func() {
		_ = rootDir.Close()
	}()

	err = fs.WalkDir(rootDir.FS(), ".", func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if shouldSkipDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipFile(entry.Name()) {
			return nil
		}

		relativePath := strings.TrimPrefix(filePath, "./")
		if !matchesAny(include, relativePath, path.Base(relativePath)) {
			return nil
		}

		data, err := rootDir.ReadFile(relativePath)
		if err != nil {
			return fmt.Errorf("read %q: %w", relativePath, err)
		}

		updated, replacements := replaceManaged(string(data), version, imageVersion)
		if replacements > 0 {
			if options.check {
				stale += replacements
			} else if err := writeFile(rootDir, relativePath, []byte(updated)); err != nil {
				return fmt.Errorf("write %q: %w", relativePath, err)
			}

			output.files = append(output.files, fileResult{
				path:         relativePath,
				replacements: replacements,
			})
		}

		for _, match := range versionPattern.FindAllStringIndex(updated, -1) {
			found := updated[match[0]:match[1]]
			if found == version {
				continue
			}

			item := warning{
				path:    relativePath,
				line:    lineNumber(updated, match[0]),
				version: found,
			}
			key := fmt.Sprintf("%s:%d:%s", item.path, item.line, item.version)
			if _, exists := warningSeen[key]; exists {
				continue
			}

			warningSeen[key] = struct{}{}
			output.warnings = append(output.warnings, item)
		}

		return nil
	})
	if err != nil {
		return output, fmt.Errorf("scan version references: %w", err)
	}
	if stale > 0 {
		return output, fmt.Errorf("found %d stale managed version references", stale)
	}

	return output, nil
}

// replaceManaged updates every supported version-reference format in value.
func replaceManaged(value, version, imageVersion string) (string, int) {
	replacements := 0
	replace := func(pattern *regexp.Regexp, replacement func([]string) string) {
		var count int
		value, count = replacePattern(value, pattern, replacement)
		replacements += count
	}

	replace(refPattern, func(_ []string) string { return "?ref=" + version })
	replace(imagePattern, func(match []string) string {
		suffix := ""
		if strings.HasSuffix(match[0], "-debug") {
			suffix = "-debug"
		}

		return "kube-dump:" + imageVersion + suffix
	})
	replace(modulePattern, func(_ []string) string { return "kube-dump@" + version })
	replace(releasePattern, func(_ []string) string { return "releases/download/" + version + "/kube-dump" })
	replace(newTagPattern, func(match []string) string {
		return "newTag:" + match[1] + imageVersion
	})

	return value, replacements
}

// replacePattern applies a replacement function and counts changed matches.
func replacePattern(value string, pattern *regexp.Regexp, replacement func([]string) string) (string, int) {
	replacements := 0
	updated := pattern.ReplaceAllStringFunc(value, func(match string) string {
		replaced := replacement(pattern.FindStringSubmatch(match))
		if replaced != match {
			replacements++
		}

		return replaced
	})

	return updated, replacements
}

// matchesAny reports whether any pattern matches any supplied path representation.
func matchesAny(patterns []string, values ...string) bool {
	for _, pattern := range patterns {
		for _, value := range values {
			matched, err := path.Match(filepath.ToSlash(pattern), value)
			if err == nil && matched {
				return true
			}
		}
	}

	return false
}

// shouldSkipDirectory reports whether a directory is outside the update scan.
func shouldSkipDirectory(name string) bool {
	switch name {
	case ".git", ".github", ".tmp", "build", "site", "vendor":
		return true
	default:
		return false
	}
}

// shouldSkipFile reports whether a file is excluded from version updates.
func shouldSkipFile(name string) bool {
	return name == "CHANGELOG.md"
}

// lineNumber converts a byte offset in value to a one-based line number.
func lineNumber(value string, offset int) int {
	return 1 + strings.Count(value[:offset], "\n")
}

// writeFile preserves the existing permissions while replacing a file below root.
func writeFile(root *os.Root, path string, data []byte) error {
	info, err := root.Stat(path)
	if err != nil {
		return err
	}
	if err := root.WriteFile(path, data, info.Mode().Perm()); err != nil {
		return err
	}

	return nil
}
