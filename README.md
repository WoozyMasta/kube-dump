<!-- markdownlint-disable MD033 MD041 -->
<div align="right">
  <p>
    <a href="./README.md">English</a> ·
    <a href="./README.ru.md">Русский</a> ·
    <a href="./README.zh.md">中文</a>
  </p>
</div>
<div align="center">
  <img src="./docs/assets/images/logo-wide.png" alt="kube-dump" width="600">
  <p>
    <a href="https://kube-dump.woozymasta.ru/en/">Documentation site</a> ·
    <a href="./deploy/README.md">Run in Kubernetes</a> ·
    <a href="./docs/README.md">Project documentation</a>
  </p>
</div>
<!-- markdownlint-enable MD033 -->

# kube-dump

**kube-dump** is a utility for preserving the actual state
of a Kubernetes environment when it needs to be captured.

In a test namespace, developer sandbox, temporary staging environment,
or demonstration environment, state is often changed manually.
Someone updates a Deployment, creates a ConfigMap, tries a new operator,
writes data to a PVC, or runs a temporary image.
That state may be needed again later,
even if the environment has already changed or been deleted.

These environments do not always have GitOps,
a separate backup system, or a strict lifecycle.
That is fine, but important state can still be left without a portable copy.

kube-dump preserves this state in a readable format that can be stored locally,
in Git, or in S3-compatible storage and used for recovery.

## Installation

Ready-to-use files for the latest stable release:

OS / architecture | Linux         | macOS         | Windows
----------------- | ------------- | ------------- | ---------------
amd64             | [Linux amd64] | [macOS amd64] | [Windows amd64]
arm64             | [Linux arm64] | [macOS arm64] | [Windows arm64]

All published versions are available on [GitHub Releases][ghr].

With Go 1.27 or newer, install the current version with:

```shell
go install github.com/woozymasta/kube-dump/v2/cmd/kube-dump@v2.0.0-rc.1
```

For running inside Kubernetes, use the ready-made
[Kustomize components][kubernetes].

The container image is published to:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1
docker.io/woozymasta/kube-dump:2.0.0-rc.1
```

For CI, debugging, and scripting, use the image with a shell and additional
utilities:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
docker.io/woozymasta/kube-dump:2.0.0-rc.1-debug
```

More installation options are described in the [documentation][docs].

## What it preserves

kube-dump preserves three independent parts of Kubernetes state:

* Kubernetes resources - YAML manifests for storing in Git, S3, or an archive;
* PVC data - file archives that can be read from a CSI snapshot;
* container images - a standard OCI Image Layout,
  including temporary images and merge request builds.

Each layer can be saved separately or together with the others.

## How to use it

Each data type has its own command. For example, save resources locally:

```sh
kube-dump resource save dir ./backup
```

Or send them to S3-compatible storage:

```sh
kube-dump resource save s3 --s3-uri s3://backup/my-cluster/
```

Save PVC data and container images with the `pvc` and `image` commands.

Capture the state before maintenance or an experiment,
before cleaning a temporary registry, or before deleting a test environment.

## What it does not replace

kube-dump does not replace Argo CD, Flux, Velero,
database operators, or an application's own backup mechanisms.

If an application can create a correct logical database backup,
continue to use that mechanism.
If infrastructure is fully declared in Git,
Git remains the source of desired state.

kube-dump captures what actually exists in the cluster
at a given moment in a readable and portable form,
so it can be used as a basis for recovery.

## Profiles and data protection

Profiles define how resources, PVCs, and images are selected and processed.
For Kubernetes manifests, they also define cleanup rules and fields to encrypt.

Built-in profiles:

* [backup] - backs up configuration, access control,
  and common operator objects while removing server-generated state;
* [export] - creates clean,
  portable manifests without runtime metadata and empty fields;
* [raw] - saves all discovered objects without cleanup or filtering.

Sensitive YAML values can be encrypted with age or deterministic AES-256-SIV.
Resource and PVC archives can additionally be protected with age.

## More information

This README is not the complete documentation.
Installation, commands, profiles, encryption, PVCs,
and images are covered in the [documentation][docs].

<!-- links -->

[docs]: https://kube-dump.woozymasta.ru/en/
[ghr]: https://github.com/WoozyMasta/kube-dump/releases
[kubernetes]: ./deploy/README.md
[backup]: ./internal/profile/builtin/backup.yaml
[export]: ./internal/profile/builtin/export.yaml
[raw]: ./internal/profile/builtin/raw.yaml

[Linux amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-linux-amd64
[Linux arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-linux-arm64
[macOS amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-darwin-amd64
[macOS arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-darwin-arm64
[Windows amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-windows-amd64.exe
[Windows arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-windows-arm64.exe
