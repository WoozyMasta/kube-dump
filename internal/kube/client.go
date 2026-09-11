// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package kube contains Kubernetes API access used by kube-dump.
package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/woozymasta/kube-dump/v2/internal/retry"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	_ "k8s.io/client-go/plugin/pkg/client/auth/exec" // Register exec auth for kubeconfig
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc" // Register OIDC auth for kubeconfig
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ClientOptions configures Kubernetes API access.
type ClientOptions struct {
	Kubeconfig            string        `long:"kubeconfig" description:"Use this kubeconfig file without changing the user's current context" short:"K"`
	Context               string        `long:"context" description:"Select this kubeconfig context without modifying the kubeconfig" short:"C"`
	Burst                 int           `long:"burst" description:"Maximum temporary burst of Kubernetes API requests" validate-min:"0" default:"100"`
	Timeout               time.Duration `long:"timeout" description:"Timeout applied to each Kubernetes API request" short:"t" validate-min:"0"`
	QPS                   float32       `long:"qps" description:"Average Kubernetes API request rate allowed by the client" validate-min:"0" default:"50"`
	InsecureSkipTLSVerify bool          `long:"insecure-skip-tls-verify" description:"Disable Kubernetes TLS certificate verification; use only for trusted endpoints" short:"k" auto-env:"false"`
}

// Client groups the dynamic and discovery clients used by collection and PVC
// backup operations.
type Client struct {
	// Dynamic accesses arbitrary Kubernetes resources.
	Dynamic dynamic.Interface
	// Discovery accesses API groups and resource metadata.
	Discovery discovery.DiscoveryInterface
	// Config is the resolved REST configuration used by both clients.
	Config *rest.Config
}

// NewClient loads Kubernetes configuration and creates API clients.
func NewClient(ctx context.Context, options ClientOptions) (*Client, error) {
	if ctx == nil {
		return nil, errors.New("kubernetes client context is required")
	}

	config, err := loadConfig(ctx, options)
	if err != nil {
		return nil, err
	}

	// A zero option means that the caller did not request an override.
	// Leaving the REST config untouched lets client-go apply its documented defaults.
	if options.QPS != 0 {
		config.QPS = options.QPS
	}
	if options.Burst != 0 {
		config.Burst = options.Burst
	}
	if options.Timeout != 0 {
		config.Timeout = options.Timeout
	}

	config.Wrap(func(transport http.RoundTripper) http.RoundTripper {
		return retry.NewTransport(transport, retry.Config{Attempts: retry.Attempts(ctx)})
	})

	// Clearing CA settings avoids retaining a contradictory custom CA
	// while explicitly disabling certificate verification.
	if options.InsecureSkipTLSVerify {
		config.Insecure = true
		config.CAData = nil
		config.CAFile = ""

		log.Logger.Warn().
			Str("component", "kubernetes").
			Bool("verify_tls", false).
			Msg("Kubernetes TLS certificate verification is disabled")
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create dynamic Kubernetes client: %w", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes discovery client: %w", err)
	}

	return &Client{Dynamic: dynamicClient, Discovery: discoveryClient, Config: config}, nil
}

// loadConfig resolves explicit kubeconfig, default kubeconfig,
// or in-cluster credentials in that order.
func loadConfig(ctx context.Context, options ClientOptions) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if options.Kubeconfig != "" {
		loadingRules.ExplicitPath = options.Kubeconfig
	}

	overrides := &clientcmd.ConfigOverrides{}
	overrides.CurrentContext = options.Context
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err == nil {
		return config, nil
	}
	if options.Kubeconfig != "" || options.Context != "" {
		return nil, fmt.Errorf("load explicitly selected Kubernetes config: %w", err)
	}

	// Default kubeconfig is unavailable in a Pod;
	// use in-cluster credentials as the fallback
	// while preserving the original config error if that fails too.
	inClusterConfig, inClusterErr := rest.InClusterConfig()
	if inClusterErr != nil {
		return nil, fmt.Errorf("load Kubernetes config: %w; in-cluster fallback: %v", err, inClusterErr)
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("load Kubernetes config: %w", ctx.Err())
	default:
		return inClusterConfig, nil
	}
}
