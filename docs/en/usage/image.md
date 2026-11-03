# Container images

The `image` command saves images used by Pods.

This complements YAML and PVC data.
A manifest may retain a tag,
but the image may disappear from the registry later
or the tag may point to different content.

See the complete list of options in the [CLI reference](../../cli.md#image).

## Saving

Save images to a local directory:

```shell
kube-dump image save dir ./backup
```

Save images to S3:

```shell
kube-dump image save s3 --s3-uri s3://backup/prod/
```

Images are stored in the standard OCI Image Layout:

```text
images/
  oci-layout
  index.json
  blobs/
```

kube-dump does not introduce a custom image format.

When saving to the same S3 prefix again, existing blobs are reused by digest.
`index.json` is published only after the capture completes successfully.
`image download s3` and `image push s3`
read only the graph reachable from this index;
old orphaned blobs are not downloaded.
The S3 index is updated conditionally,
so concurrent captures produce an explicit conflict
instead of silently replacing another writer's index.

Image indexes larger than 16 MiB
or containing more than 100,000 top-level manifests are rejected by validation.
This protects inspect and publication from unbounded metadata.
For a large failed capture,
the command retains up to eight detailed image errors
and reports the number of additional omitted details;
failure counts still include every image.

## How images are selected

kube-dump obtains image references from these fields:

```text
spec.containers[].image
status.containerStatuses[].imageID
```

The same fields are read from init containers.

`spec.containers[].image` preserves the original name, for example:

```text
ghcr.io/acme/api:mr-123
```

`imageID` allows kube-dump to use the digest
that is actually running when it is available.

The saved OCI layout keeps the declared `spec.containers[].image` reference
as a source alias and records the runtime digest separately when available.
They can differ: the alias describes what the Pod declared,
while the digest identifies the content that was running.

> [!WARNING]
> If the exact digest cannot be used,
> kube-dump falls back to the original `spec` reference and writes a warning.
> The mutable tag may already point to another image by then.

## Namespaces

```shell
kube-dump image save dir ./backup --namespace production
```

The flag can be repeated.

## Platforms

Without `--image-platform`,
the platform variant used by the running container is saved.

To save additional platform variants, list them explicitly:

```shell
kube-dump image save dir ./backup \
  --image-platform linux/amd64 --image-platform linux/arm64
```

If an image does not have every requested platform,
kube-dump fails the capture and keeps the previous saved layout unchanged.
Each distinct mutable source tag is resolved once at the start of the capture.
All requested platforms for that tag are copied from its pinned result;
the same tag is not resolved again for each platform.

## Registry access

By default, kube-dump uses credentials available where it is running.

You can additionally use `imagePullSecrets` from Pods:

```shell
kube-dump image save dir ./backup --image-pull-secrets
```

> [!NOTE]
> This helps with ordinary Kubernetes Secret configurations
> but does not replace every possible kubelet authentication mechanism.

In practice, the kube-dump process must be able to access the registries.
The `--image-pull-secrets` option is an additional credential source.

## Registry mirror

Replace a source registry host with a mirror by supplying a mapping:

```shell
kube-dump image save dir ./backup \
  --registry-mirror registry.old.example=registry.mirror.example
```

The original reference remains known, while downloads use the mirror.

## Profile selection

For example,
this profile selects images from Pods in the `production` namespace:

```yaml
apiVersion: kube-dump/v2
kind: Profile

metadata:
  name: production
  description: Production image backup

resources: {}

images:
  selection:
    namespaces:
      - production
```

Pods can be selected by labels, annotations, and owners;
image references can be selected by registry, path, and tag.

The following profile selects images only from Pods owned by Deployments:

```yaml
images:
  selection:
    owners:
      resources:
        - apps/v1/deployments
    containerTypes:
      - container
```

`owners` checks the Pod's `ownerReferences` chain,
not the Deployment name itself.
The bundled image RBAC covers standard `apps` and `batch` workload owners.
Profiles that select custom owner resources need matching `get` permissions.

The following fragment limits image references:

```yaml
images:
  selection:
    references:
      - registry: ghcr.io
        repository: acme/*
        tags:
          - 'release-*'
```

See the complete format in the
[profile reference](../../profile.md#imagepolicy).

## Inspecting

Inspect a local image copy:

```shell
kube-dump image inspect dir ./backup
```

Inspect a copy in S3:

```shell
kube-dump image inspect s3 --s3-uri s3://backup/prod/
```

The command shows saved references, digests, platforms, and data size.

## Publishing to another registry

Publish a local copy:

```shell
kube-dump image push dir ./backup registry.example.com/recovered
```

Publish a copy from S3:

```shell
kube-dump image push s3 registry.example.com/recovered --s3-uri s3://backup/prod/
```

To publish only selected images, pass references after the target registry.
Without additional references, all images are published:

```shell
kube-dump image push dir ./backup registry.example.com/recovered \
  ghcr.io/acme/api:2.5-rc.11
```

All selected references are validated before publishing starts,
so a typo does not result in a partially completed publication.

The target registry is explicit.
kube-dump never publishes back to the source registry automatically.

For complex copying, tag remapping, signatures, and other operations,
use [ORAS], [crane], or [regctl].

<!-- links -->

[ORAS]: https://github.com/oras-project/oras
[crane]: https://github.com/google/go-containerregistry
[regctl]: https://github.com/regclient/regclient
