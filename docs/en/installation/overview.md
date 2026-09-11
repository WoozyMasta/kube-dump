# Installation and usage

kube-dump is distributed as a single executable and as a container image.
It can be run manually, from CI, or inside Kubernetes.

## Installing the executable

Ready-made binaries for the latest stable release:

Architecture/OS | Linux         | macOS         | Windows
--------------- | ------------- | ------------- | ---------------
amd64           | [Linux amd64] | [macOS amd64] | [Windows amd64]
arm64           | [Linux arm64] | [macOS arm64] | [Windows arm64]

All published versions are available on [GitHub Releases].

With Go 1.27 or newer, install the current version with:

```shell
go install github.com/woozymasta/kube-dump/v2/cmd/kube-dump@latest
```

Verify the installation:

```shell
kube-dump version
```

## Running locally

By default, kube-dump uses kubeconfig and the current Kubernetes context:

```shell
kube-dump resource save dir ./backup
```

Specify another kubeconfig or context explicitly:

```shell
kube-dump resource save dir ./backup \
  --kubeconfig ~/.kube/config --context production
```

See [Kubernetes deployment](kubernetes.md) for installation in Kubernetes
and [CI integration](ci.md) for running from CI.

## Container images

Images are published in these registries:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1
docker.io/woozymasta/kube-dump:2.0.0-rc.1
```

A debug variant with a shell and utilities for debugging
and CI scripting is published separately:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
docker.io/woozymasta/kube-dump:2.0.0-rc.1-debug
```

Pin a specific image version for automation
instead of the floating `latest` tag.

The main image is minimal and intended to run kube-dump.
The `-debug` suffix denotes an image with a shell
and utilities for CI scripts and diagnostics.

## Permissions

Kubernetes permissions depend on the command and selected profile.
In your Kustomize composition, select the minimal component:
`components/rbac/resource`, `components/rbac/image`, or `components/rbac/pvc`.
See [Kubernetes deployment](kubernetes.md) for details.

<!-- links -->

[Linux amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-linux-amd64
[Linux arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-linux-arm64
[macOS amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-darwin-amd64
[macOS arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-darwin-arm64
[Windows amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-windows-amd64.exe
[Windows arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-windows-arm64.exe
[GitHub Releases]: https://github.com/WoozyMasta/kube-dump/releases
