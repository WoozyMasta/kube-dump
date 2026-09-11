// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/woozymasta/kube-dump/v2/internal/export"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	"github.com/woozymasta/kube-dump/v2/internal/state/codec"
)

// resourceStdoutSink buffers normalized objects until collection completes.
// Buffering lets stdout use the same deterministic path order as directory output.
type resourceStdoutSink struct {
	output    io.Writer
	spool     *os.File
	paths     map[string]struct{}
	documents []resourceStdoutDocument
	mutex     sync.Mutex
}

// resourceStdoutDocument contains one serialized object and its canonical path.
type resourceStdoutDocument struct {
	path   string
	offset int64
	size   int64
}

// disableProgress marks stdout capture as a pipeline command without a useful interactive progress display.
func (*ResourcesStdoutCommand) disableProgress() {}

// Execute captures selected Kubernetes objects and writes them to stdout as multi-document YAML.
// Each document is preceded by its canonical backup path.
func (c *ResourcesStdoutCommand) Execute(_ []string) error {
	if c.output == nil {
		return errors.New("resource output is required")
	}

	prepared, err := prepareResourceCollection(c.resourceOptions, c.selection, "")
	if err != nil {
		return fmt.Errorf("prepare resource output: %w", err)
	}

	sink := newResourceStdoutSink(c.output)
	defer func() { _ = sink.Close() }()

	compiled, result, err := collectResources(
		c.context(),
		sink,
		c.progress,
		c.resourceOptions,
		prepared,
		c.clientOptions,
	)
	if err != nil {
		return fmt.Errorf("collect Kubernetes resources: %w", err)
	}
	if err := requireCompleteResourceCollection(result); err != nil {
		return err
	}
	if err := sink.Flush(c.context()); err != nil {
		return fmt.Errorf("write resource output: %w", err)
	}

	logResourceBackup("stdout", "-", compiled, result, c.progress)

	return nil
}

// newResourceStdoutSink creates a sink for one process output stream.
func newResourceStdoutSink(output io.Writer) *resourceStdoutSink {
	return &resourceStdoutSink{output: output, paths: make(map[string]struct{})}
}

// Write buffers one object with the path it would have in a directory backup.
// stdout has no previous state to compare against, so changed is always false.
func (s *resourceStdoutSink) Write(ctx context.Context, object state.Object) (changed bool, err error) {
	if s == nil || s.output == nil {
		return false, errors.New("resource stdout sink is not initialized")
	}
	if ctx == nil {
		return false, errors.New("resource stdout context is required")
	}

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}

	data, err := codec.Marshal(object)
	if err != nil {
		return false, err
	}

	path, err := export.CanonicalObjectPath(object.Identity)
	if err != nil {
		return false, err
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, exists := s.paths[path]; exists {
		return false, fmt.Errorf("duplicate canonical resource path %q", path)
	}

	if s.spool == nil {
		spool, err := os.CreateTemp("", "kube-dump-resource-stdout-")
		if err != nil {
			return false, fmt.Errorf("create resource stdout spool: %w", err)
		}

		s.spool = spool
	}

	offset, err := s.spool.Seek(0, io.SeekEnd)
	if err != nil {
		return false, fmt.Errorf("seek resource stdout spool: %w", err)
	}
	if _, err := s.spool.Write(data); err != nil {
		return false, fmt.Errorf("write resource stdout spool: %w", err)
	}

	s.documents = append(s.documents, resourceStdoutDocument{
		path:   path,
		offset: offset,
		size:   int64(len(data)),
	})
	s.paths[path] = struct{}{}

	return false, nil
}

// Flush writes all buffered documents in canonical path order after collection succeeds.
func (s *resourceStdoutSink) Flush(ctx context.Context) error {
	if s == nil || s.output == nil {
		return errors.New("resource stdout sink is not initialized")
	}
	if ctx == nil {
		return errors.New("resource stdout context is required")
	}

	s.mutex.Lock()
	documents := append([]resourceStdoutDocument(nil), s.documents...)
	s.mutex.Unlock()

	sort.Slice(documents, func(left, right int) bool {
		return documents[left].path < documents[right].path
	})
	if s.spool == nil {
		return nil
	}

	for _, document := range documents {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if _, err := fmt.Fprintf(s.output, "---\n# ./%s\n", document.path); err != nil {
			return fmt.Errorf("write resource path comment: %w", err)
		}
		if _, err := io.Copy(s.output, io.NewSectionReader(s.spool, document.offset, document.size)); err != nil {
			return fmt.Errorf("write resource YAML: %w", err)
		}
	}

	return nil
}

// Close removes the temporary spool after output has been flushed or abandoned.
func (s *resourceStdoutSink) Close() error {
	if s == nil {
		return nil
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.spool == nil {
		return nil
	}

	name := s.spool.Name()
	err := s.spool.Close()
	s.spool = nil
	if removeErr := os.Remove(name); err == nil {
		err = removeErr
	}

	return err
}
