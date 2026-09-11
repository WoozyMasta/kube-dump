package pvc

import (
	"context"
	"io"
	"reflect"
	"testing"
)

type captureExecTransport struct {
	request ExecRequest
}

func (t *captureExecTransport) Exec(_ context.Context, request ExecRequest, _ io.Reader, _, _ io.Writer) error {
	t.request = request
	return nil
}

func TestExecRequestValidation(t *testing.T) {
	t.Parallel()

	if err := (ExecRequest{Namespace: "default", Pod: "helper", Command: []string{"tar"}}).Validate(); err != nil {
		t.Fatalf("ExecRequest.Validate() error = %v", err)
	}
	if err := (ExecRequest{Namespace: "default", Pod: "helper"}).Validate(); err == nil {
		t.Fatal("ExecRequest.Validate() accepted an empty command")
	}
}

func TestRESTExecTransportValidation(t *testing.T) {
	t.Parallel()

	transport := RESTExecTransport{}
	if err := transport.Exec(nil, ExecRequest{}, nil, nil, nil); err == nil {
		t.Fatal("RESTExecTransport.Exec() accepted an invalid request")
	}
}

func TestPodStrategyExecUsesHelperMountPath(t *testing.T) {
	t.Parallel()

	transport := &captureExecTransport{}
	strategy := &PodStrategy{options: PodStrategyOptions{Exec: transport}}
	pod := HelperPod{
		Namespace:   "default",
		Name:        "helper",
		AgentBinary: "/kube-dump",
		Container:   "helper",
		MountPath:   "/mounted volume",
	}

	want := []string{"/kube-dump", "volume", "export", "/mounted volume"}
	if err := strategy.exec(context.Background(), pod, want, nil, io.Discard); err != nil {
		t.Fatalf("PodStrategy.exec() error = %v", err)
	}
	if !reflect.DeepEqual(transport.request.Command, want) {
		t.Fatalf("exec command = %#v, want %#v", transport.request.Command, want)
	}
}
