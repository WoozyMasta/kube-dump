# Operational scenarios

This page collects practical recommendations
for organizing and operating kube-dump backups.

## Multiple profiles

One cluster can use different profiles on different schedules.
For example, one profile selects the `dev-*` namespaces and runs frequently,
while another selects `test-*` and runs less often.

Both profiles can use the same directory:

```shell
kube-dump resource save dir ./backup --profile ./profiles/dev.yaml
kube-dump pvc save dir ./backup --profile ./profiles/dev.yaml
kube-dump image save dir ./backup --profile ./profiles/dev.yaml
```

The `test` profile only changes the profile file:

```shell
kube-dump resource save dir ./backup --profile ./profiles/test.yaml
kube-dump pvc save dir ./backup --profile ./profiles/test.yaml
kube-dump image save dir ./backup --profile ./profiles/test.yaml
```

Without `--prune`, these runs add to the existing result.
Resources and images are not removed
just because another profile did not select them.
PVCs from different namespaces are stored in different subdirectories as well.

If independent schedules can run at the same time,
follow the rules in [Concurrent runs][].

> [!CAUTION]
> Do not use `--prune` for independent runs sharing one directory.
> A later `dev` run can remove the results produced by `test`.

## Multiple clusters

The usual approach is to give each cluster its own directory.
This is the simplest option:
data from different clusters stays separate,
and all three backup types have one cluster root.

```text
backup/
├── cluster-a/
│   ├── resources/
│   ├── volumes/
│   └── images/
└── cluster-b/
    ├── resources/
    ├── volumes/
    └── images/
```

Example commands for `cluster-a`:

```shell
kube-dump resource save dir ./backup/cluster-a
kube-dump pvc save dir ./backup/cluster-a
kube-dump image save dir ./backup/cluster-a
```

If clusters use the same images,
the image directory can be moved to a shared root.
OCI layout stores content by digest,
so common layers are reused instead of being stored multiple times.

```text
backup/
├── cluster-a/
│   ├── resources/
│   └── volumes/
├── cluster-b/
│   ├── resources/
│   └── volumes/
└── images/
```

> [!CAUTION]
> Use the shared root only for images.
> Do not put resources or PVCs from different clusters there,
> even if each profile currently selects a unique namespace.
> The command may still complete, but cluster-scoped resources,
> identical namespaces or PVCs, and future profile changes can overwrite data.
>
> A namespace is not an isolation boundary between clusters.

In this layout, image commands use `./backup`:

```shell
kube-dump image save dir ./backup
```

The same principle applies to S3:
use different prefixes for resources and PVCs, and a shared prefix for images.
With separate clusters, do not mix their resources or PVCs under one prefix.

If runs for different clusters may overlap,
follow the same rules in [Concurrent runs][].

## Concurrent runs

If two processes write to the same directory or S3 prefix at the same time,
the result depends on the data type.

* For local images, the second `image save dir`
  waits until the first one finishes.
  This protects the shared OCI layout from concurrent writes.
* Resource saves use the same kind of writer lock.
  Different resource directories can be written in parallel.
* PVCs can be saved in parallel when the selected sets do not overlap.
  Avoid running two jobs for the same PVC at the same time:
  they can modify the same files concurrently.
* There is no shared filesystem lock between machines.
  For S3 `image save`, kube-dump reads the current index
  and checks during publication that it has not changed.
  If another process saved changes first, the second run fails
  with a conflict instead of overwriting the published index.
* `image save s3` merges references into the shared OCI index
  but does not remove old blobs.

Local locks use empty marker files named
`.kube-dump-resource-writer-*.lock` and `.kube-dump-image-writer-*.lock`.
They remain in the parent directory after the command finishes;
the actual lock is held by the file descriptor and is released on close.
When no `kube-dump` process is using the destinations,
you can remove these marker files manually.

> [!NOTE]
> The presence of a `.lock` file does not mean that the destination is locked.
> Do not remove it while `kube-dump` is running,
> because that can break protection against concurrent writes.

## Removing old results

In `resource save` and local `image save`,
`--prune` enables destructive cleanup.
After a successful save, kube-dump removes anything
that is not in the current selection.

For `resource save s3`, the result is also built from the current selection,
but old generations are removed later:
the current and previous generations are retained for readers.

This lets you remove data that was deleted from the cluster
after the previous run.

> [!CAUTION]
> Use `--prune` only for one backup with a stable, non-dynamic configuration.
> No other profiles, clusters, or schedules should write to the target,
> and namespaces and selectors must not change between runs.
>
> Otherwise a later run removes results that are outside its current selection.

Do not put arbitrary files inside the managed `resources/`, `volumes/`,
or `images/` subdirectories if they have a different lifecycle from the backup.
Use a neighboring top-level directory for custom files.

[Concurrent runs]: #concurrent-runs
