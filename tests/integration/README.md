# Integration tests

These tests build and execute the current `kube-dump` binary
against an isolated Kubernetes cluster.
They are not included in the default `go test ./...` run.

The host needs the Go toolchain, GNU Make, and a running Docker engine.
kind uses Docker directly to run Kubernetes node containers;
no Docker-in-Docker layer or Kubernetes runtime installation is required.
Podman is supported as an explicit alternative.

The tests start an isolated kind cluster
and launch registry and S3 fixtures only when a test needs them.
The PVC test builds the current CLI as a static Linux helper image
and loads it into kind; it does not use the release image.
Set `KUBE_DUMP_INTEGRATION_VOLUME_IMAGE` to use a preloaded helper image.

Registry and SeaweedFS image references can be overridden with
`KUBE_DUMP_REGISTRY_IMAGE` and `KUBE_DUMP_SEAWEEDFS_IMAGE`
when the required images are supplied by a local cache or an internal registry.

Run the suite with:

```shell
make integration
```

Docker is the default provider.
To select Podman explicitly, use:

```shell
make integration INTEGRATION_RUNTIME=podman
```

The tests use `kind` from `PATH` by default.
To install the pinned kind binary locally, run:

```shell
make -C tests/integration kind-install
```

The downloaded binary is stored under `tests/integration/.tools`
and takes precedence over `PATH` on subsequent runs.
To use another kind binary, pass its path explicitly:

```shell
make integration KIND=/d/bin/kind.exe
```

The `kind-install` target uses the host `GOOS` and `GOARCH`
and downloads the matching release asset from GitHub.
It requires `curl`.

The integration target runs `go test` in verbose mode,
so each test reports its `RUN`, `PASS`,
or `FAIL` status while the suite is running.

The harness creates a temporary kubeconfig
and never changes the user's current Kubernetes context.
Set `KUBE_DUMP_KEEP_INTEGRATION_ENV=1`
to keep the cluster and temporary files for local debugging after a test run.
