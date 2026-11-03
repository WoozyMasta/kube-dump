package kube

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestLoadConfigUsesRequestedContextWithoutMutatingKubeconfig(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config")
	config := &clientcmdapi.Config{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: "cluster-a",
		Clusters: map[string]*clientcmdapi.Cluster{
			"cluster-a": {Server: "https://cluster-a.example"},
			"cluster-b": {Server: "https://cluster-b.example"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"cluster-a": {},
			"cluster-b": {},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"cluster-a": {Cluster: "cluster-a", AuthInfo: "cluster-a"},
			"cluster-b": {Cluster: "cluster-b", AuthInfo: "cluster-b"},
		},
	}
	if err := clientcmd.WriteToFile(*config, configPath); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read original kubeconfig: %v", err)
	}

	loaded, err := loadConfig(context.Background(), ClientOptions{
		Kubeconfig: configPath,
		Context:    "cluster-b",
	})
	if err != nil {
		t.Fatalf("load requested context: %v", err)
	}
	if loaded.Host != "https://cluster-b.example" {
		t.Fatalf("loaded host = %q, want cluster-b host", loaded.Host)
	}

	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read kubeconfig after load: %v", err)
	}
	if !bytes.Equal(current, original) {
		t.Fatal("loading a context modified the kubeconfig")
	}
}

func TestLoadConfigDoesNotFallbackForExplicitTargetErrors(t *testing.T) {
	t.Parallel()

	if _, err := loadConfig(context.Background(), ClientOptions{
		Kubeconfig: filepath.Join(t.TempDir(), "missing-config"),
	}); err == nil {
		t.Fatal("loadConfig() silently fell back after an explicit kubeconfig error")
	}
	if _, err := loadConfig(context.Background(), ClientOptions{Context: "missing-context"}); err == nil {
		t.Fatal("loadConfig() silently fell back after an explicit context error")
	}
}
