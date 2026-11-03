# kube-dump documentation

kube-dump is a cross-platform utility for backing up
and exporting Kubernetes resources, PVC data, and container images.

Choose a language:

* [English](en/README.md)
* [Русский](ru/README.md)
* [中文](zh/README.md)

Each language section includes:

* installation and Kubernetes/CI deployment;
* usage guides for resources, PVC data, and images;
* profiles, encryption, and recovery;
* CLI and profile references.

Useful project files:

* [CLI reference](cli.md);
* [profile reference](profile.md);
* [example profile](example.profile.yaml);
* [profile JSON Schema](../pkg/profile/schema/profile.schema.json);
* built-in profiles:
  * [backup](../internal/profile/builtin/backup.yaml),
  * [export](../internal/profile/builtin/export.yaml),
  * [raw](../internal/profile/builtin/raw.yaml).

The generated documentation site is available at
[kube-dump.woozymasta.ru](https://kube-dump.woozymasta.ru/).
