<!-- markdownlint-disable MD024 -->
# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog][],
and this project adheres to [Semantic Versioning][].

<!--
## Unreleased

### Added
### Changed
### Removed
-->

## [2.0.0][] - 2026-09-11

### Added

* Backup and restore support for PVC filesystems, including CSI snapshots.
* Container image backup and publishing using the standard OCI Image Layout.
* S3-compatible storage for Kubernetes resources,
  PVC backups, and container images.
* Built-in and custom profiles with reusable selection rules for resources,
  PVCs, OCI images, and processing rules for Kubernetes manifests.
* AGE encryption for individual fields in saved Kubernetes manifests
  and for entire resource and PVC archive files.
* AES-256-SIV deterministic encryption for individual fields in manifests,
  avoiding unnecessary changes when the source value stays the same.

### Changed

* kube-dump was completely rewritten in Go as a cross-platform static binary.
  The v2 CLI and configuration are not compatible with v1.
* v2 stores Kubernetes resources in a canonical
  `group/version/resource/namespace/name.yaml` layout.
* Resource archives now use gzip or zstd compression;
  xz and bzip2 support from v1 was removed.
* v2 is licensed under the MIT License.

[2.0.0]: https://github.com/WoozyMasta/a2s/compare/1.1.2...v2.0.0

## [1.1.2](https://github.com/WoozyMasta/kube-dump/releases/tag/1.1.2) - 2022-05-19

### Changed

* Update kubectl 1.23.3 -> 1.24.0

## [1.1.1](https://github.com/WoozyMasta/kube-dump/releases/tag/1.1.1) - 2022-02-10

### Changed

* fixed the sed regex to match the hyphen in `git_remote_url`
  [#30](https://github.com/WoozyMasta/kube-dump/pull/30)

## [1.1.0](https://github.com/WoozyMasta/kube-dump/releases/tag/1.1.0) - 2022-02-09

### Added

* option `--detailed` for not remove detailed state specific fields in files
  [#29](https://github.com/WoozyMasta/kube-dump/pull/29)
* option `--output-by-type` for organize output into directories by resource type
  [#29](https://github.com/WoozyMasta/kube-dump/pull/29)
* option `--flat` for organize all resources of the same type in the same file
  [#29](https://github.com/WoozyMasta/kube-dump/pull/29)
* package `bind-tools` for fix some DNS resolution issues

### Changed

* Base Alpine image version updated to **3.15** from **3.13**
* `yq` and `jq` install from Alpine repository
* `kubectl` update to **1.23.3** from **1.23.1**

## [1.0.7](https://github.com/WoozyMasta/kube-dump/releases/tag/1.0.7) - 2022-01-18

### Added

* Publish image to ghcr container registry
* Publish image to quay.io container registry

### Changed

* Fix dump of cluster resources to be single item per file
  [#22](https://github.com/WoozyMasta/kube-dump/pull/22)
* Delete always changing annotations from HPAs
  [#23](https://github.com/WoozyMasta/kube-dump/pull/23)
* Fix deleting multiple archive files
  [#28](https://github.com/WoozyMasta/kube-dump/pull/28)
* Update kubectl 1.23.1
* Update yq 4.16.2.
* Update GitHub Actions
* Fix typos on docs
* Semver versioning, remove `v` from git tags

## [v1.0.6](https://github.com/WoozyMasta/kube-dump/releases/tag/v1.0.6) - 2021-05-23

### Changed

* Replaced busybox grep with GNU grep
  [#19](https://github.com/WoozyMasta/kube-dump/issues/19)
* Updated kubectl to 1.21.1
* Updated yq to 4.9.3

## [v1.0.5](https://github.com/WoozyMasta/kube-dump/releases/tag/v1.0.5) - 2021-04-06

### Changed

* Fixed parameter --kube-insecure-tls
  [#15](https://github.com/WoozyMasta/kube-dump/issues/15)
* Fixed missing header issue in ServiceAccount/Rolebinding.
  [#16](https://github.com/WoozyMasta/kube-dump/issues/16)

## [v1.0.4](https://github.com/WoozyMasta/kube-dump/releases/tag/v1.0.4) - 2021-03-01

### Added

* Coreutils to docker image for replace busybox utils
* Automatic publish release action

### Changed

* Simple try to fix run in MacOS

## [v1.0.3](https://github.com/WoozyMasta/kube-dump/releases/tag/v1.0.3) - 2021-02-17

### Changed

* Ditching the test alpine:edge container and moving to version 3.13

## [v1.0.2](https://github.com/WoozyMasta/kube-dump/releases/tag/v1.0.2) - 2021-02-17

### Changed

* Fixed issue with relative paths
  [#6](https://github.com/WoozyMasta/kube-dump/issues/6)
* Fixed force remove resorces dirs in data dir
  [#7](https://github.com/WoozyMasta/kube-dump/issues/7)
* Fixed tty detection for output redirects, show plain log messages if not use
interactive seesion or redirect output to file

## [v1.0.1](https://github.com/WoozyMasta/kube-dump/releases/tag/v1.0.1) - 2021-02-17

### Changed

* Fixed issue with trusting keys when first ssh cloning a git repository
in a container

## [v1.0.0](/releases/tag/v1.0.0) - 2021-02-07

### Added

* Saving only those resources to which you have read access
* Can work with a list of namespaces otherwise all available ones will be used
* Can save both namespaced and cluster wide resources
* Can run locally, in a container or in a cluster
* Can archive and rotate dump archives
* Can commit dumps to a git repository and send to a remote repository
  * Use OAuth token
  * Use ssh key
* Can specify a list of resources to be dumped
* It is possible to configure via command line arguments
* It is possible to configure via environment variables

<!--links-->
[Keep a Changelog]: https://keepachangelog.com/en/1.1.0/
[Semantic Versioning]: https://semver.org/spec/v2.0.0.html
