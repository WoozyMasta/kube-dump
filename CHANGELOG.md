# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added

* Coreutils to docker image for replace busybox utils
* Automatic publish release action

### Changed

* Simple try to fix run in MacOS

## [v1.0.3](../../releases/tag/v1.0.3) - 2021-02-17

### Changed

* Ditching the test alpine:edge container and moving to version 3.13

## [v1.0.2](../../releases/tag/v1.0.2) - 2021-02-17

### Changed

* Fixed issue with relative paths [#6](../../issues/6)
* Fixed force remove resorces dirs in data dir [#7](../../issues/7)
* Fixed tty detection for output redirects, show plain log messages if not use
interactive seesion or redirect output to file
## [v1.0.1](../../releases/tag/v1.0.1) - 2021-02-17

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
