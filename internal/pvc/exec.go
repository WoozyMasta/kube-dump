// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package pvc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// RESTExecTransport implements ExecTransport with Kubernetes SPDY exec.
type RESTExecTransport struct {
	// Config is the Kubernetes REST configuration used for the exec endpoint.
	Config *rest.Config
}

// ExecRequest identifies a command execution inside a helper Pod.
type ExecRequest struct {
	// Namespace is the helper Pod namespace.
	Namespace string
	// Pod is the helper Pod name.
	Pod string
	// Container is the target container name.
	Container string
	// Command is the argv passed to the remote command.
	Command []string
}

// ExecTransport streams a command through a Kubernetes Pod exec endpoint.
type ExecTransport interface {
	Exec(ctx context.Context, request ExecRequest, stdin io.Reader, stdout, stderr io.Writer) error
}

// Validate checks that the exec request has no ambiguous API target.
func (r ExecRequest) Validate() error {
	if r.Namespace == "" || r.Pod == "" {
		return errors.New("exec namespace and pod are required")
	}
	if len(r.Command) == 0 || r.Command[0] == "" {
		return errors.New("exec command is required")
	}

	return nil
}

// Exec opens a non-TTY SPDY stream with isolated binary stdout and stderr.
func (t RESTExecTransport) Exec(
	ctx context.Context,
	request ExecRequest,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	if t.Config == nil {
		return errors.New("exec REST config is required")
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if stdout == nil || stderr == nil {
		return errors.New("exec stdout and stderr are required")
	}
	if ctx == nil {
		return errors.New("exec context is required")
	}

	config := rest.CopyConfig(t.Config)

	// Use the core API serializer explicitly because exec is a subresource of core/v1 Pods,
	// even when the caller's client config targets another group.
	config.APIPath = "/api"
	config.GroupVersion = &schema.GroupVersion{Version: "v1"}
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	client, err := rest.RESTClientFor(config)
	if err != nil {
		return fmt.Errorf("create exec REST client: %w", err)
	}

	requestURL := client.Post().
		Namespace(request.Namespace).
		Resource("pods").
		Name(request.Pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: request.Container,
			Command:   request.Command,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec).URL()
	if err := validateExecURL(requestURL); err != nil {
		return err
	}

	// TTY is disabled so stdout and stderr remain separate protocol streams;
	// callers use stderr for diagnostics and stdout for the volume payload/result.
	executor, err := remotecommand.NewSPDYExecutor(config, "POST", requestURL)
	if err != nil {
		return fmt.Errorf("create SPDY executor: %w", err)
	}
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("stream Pod exec: %w", err)
	}

	return nil
}

// validateExecURL rejects an incomplete executor URL before opening a stream.
func validateExecURL(value *url.URL) error {
	if value == nil || value.Scheme == "" || value.Host == "" {
		return errors.New("exec URL is incomplete")
	}

	return nil
}
