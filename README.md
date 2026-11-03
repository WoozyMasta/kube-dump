# kube-dump · [EN] · [RU] · [ZH]

**kube-dump is a utility for capturing Kubernetes state.**
See the [documentation site][docs] for details.

![kube-dump][logo]

In a test namespace, developer sandbox, temporary staging environment,
or demonstration environment, state is often changed manually.
Someone updates a Deployment, creates a ConfigMap, tries a new operator,
writes data to a PVC, or runs a temporary image.
That state may be needed again later,
even if the environment has already changed or been deleted.
To run kube-dump inside the cluster,
use the [Kustomize components][kubernetes].

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
go install github.com/woozymasta/kube-dump/v2/cmd/kube-dump@v2.0.0-rc.2
```

For running inside Kubernetes, use the ready-made
[Kustomize components][kubernetes].

The container image is published to:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.2
docker.io/woozymasta/kube-dump:2.0.0-rc.2
```

For CI, debugging, and scripting,
use the image with a shell and additional utilities:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.2-debug
docker.io/woozymasta/kube-dump:2.0.0-rc.2-debug
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
Browse the [documentation source][project-docs],
or visit the [documentation site][docs].
This README is also available in [Russian][RU] and [Chinese][ZH].

[docs]: https://kube-dump.woozymasta.ru/en/
[project-docs]: ./docs/README.md
[ghr]: https://github.com/WoozyMasta/kube-dump/releases
[kubernetes]: ./deploy/README.md
[logo]: ./docs/assets/images/logo-wide.png
[EN]: ./README.md
[RU]: ./README.ru.md
[ZH]: ./README.zh.md
[backup]: ./internal/profile/builtin/backup.yaml
[export]: ./internal/profile/builtin/export.yaml
[raw]: ./internal/profile/builtin/raw.yaml

[Linux amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-linux-amd64
[Linux arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-linux-arm64
[macOS amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-darwin-amd64
[macOS arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-darwin-arm64
[Windows amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-windows-amd64.exe
[Windows arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-windows-arm64.exe
