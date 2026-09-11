# PVC data

The `pvc` command saves the contents of `PersistentVolumeClaim` objects.
Each PVC is stored as a separate compressed archive,
so volume data can use a different schedule and retention period from YAML.

See the complete list of options in the [CLI reference](../../cli.md#pvc).

## Basic backup

```shell
kube-dump pvc save dir ./backup
```

Limit the backup to one namespace:

```shell
kube-dump pvc save dir ./backup --namespace production
```

Select PVCs by name or glob:

```shell
kube-dump pvc save dir ./backup --pvc postgres-data --pvc 'redis-*'
```

## Backup layout and retention

Each PVC copy is stored in a timestamped directory:

```text
volumes/
  production/
    postgres-data/
      20260904T010203Z/
        data.tar.zst
        metadata.yaml
```

The default is to keep the five latest copies of each PVC.
Change it with `--keep`:

```shell
kube-dump pvc save dir ./backup --keep 10
```

Use `--keep 0` to disable rotation and retain all copies:

```shell
kube-dump pvc save dir ./backup --keep 0
```

## Archive contract

Each copy contains `metadata.yaml` and one compressed data stream.
The metadata records the format, strategy, compression, uncompressed size,
SHA-256 digest, and encryption state. `sizeBytes` is the uncompressed size.

Archives preserve regular files, directories, relative symbolic links,
permissions, numeric UID/GID, and modification time.
They do not preserve extended attributes, ACLs, sparse files,
hard-link topology, devices, or other special files.
Files with multiple hard links are rejected,
and a Windows volume agent cannot restore non-zero numeric ownership
or guarantee complete POSIX permission-bit fidelity.

Restore validates the complete archive before writing the target.
After extraction starts, restore is not transactional
and does not roll back data already written.
Local restores use a temporary copy of the encoded artifact,
so temporary storage must fit the saved archive.
The helper validates and extracts the stream
without materializing a decoded PVC-sized copy in `/tmp`.

## `snapshot-copy` strategy

This is the default strategy:

```shell
kube-dump pvc save dir ./backup --strategy snapshot-copy
```

The flow is:

```text
source PVC
  -> VolumeSnapshot
  -> temporary PVC restored from the snapshot
  -> temporary Pod
  -> archive
```

This reads a point-in-time copy rather than the application's live volume.

> [!IMPORTANT]
> CSI snapshots and the corresponding Kubernetes permissions are required.

The temporary helper Pod uses the image for the current version.
The default pull policy is `IfNotPresent`;
use `Always` when an image was updated under the same tag:

```shell
kube-dump pvc save dir ./backup --image-pull-policy Always
```

Override the helper image or executable
with `--image` and `--helper-binary-path`.
The helper has an ephemeral `/tmp` for small runtime files;
restore validation and extraction
do not spool a decoded PVC-sized archive there.

Select a snapshot class explicitly when needed:

```shell
kube-dump pvc save dir ./backup --snapshot-class csi-snapshots
```

## `pod` strategy

If snapshots are unavailable, use:

```shell
kube-dump pvc save dir ./backup --strategy pod
```

The temporary Pod reads the source PVC directly.

> [!WARNING]
> This is less reliable for actively changing data
> because the application may keep writing during the copy.
> Prefer `snapshot-copy` for databases and
> similar workloads when the storage system supports it.

## Saving to S3

```shell
kube-dump pvc save s3 --s3-uri s3://backup/prod/
```

The temporary Pod can send the archive directly to S3.
This is important for large volumes
because the data does not pass through the kube-dump process.

> [!IMPORTANT]
> S3 must therefore be reachable from the cluster.

If S3 has a different in-cluster address, provide it:

```shell
kube-dump pvc save s3 --s3-uri s3://backup/prod/ \
  --s3-pod-endpoint http://seaweedfs.storage.svc:8333 \
  --s3-insecure
```

## Size limits

Skip an individual PVC above a limit:

```shell
kube-dump pvc save dir ./backup --max-size 500GiB
```

Set a total limit for PVCs in one namespace:

```shell
kube-dump pvc save dir ./backup --max-namespace-size 2TiB
```

## Profile selection

This profile selects PVCs in `production`:

```yaml
apiVersion: kube-dump/v2
kind: Profile

metadata:
  name: production
  description: Production PVC backup

resources: {}

pvc:
  selection:
    namespaces:
      - production
    names:
      - 'postgres-*'
      - 'redis-*'
```

Labels and annotations can also be used.
See the [profile reference](../../profile.md#pvcpolicy).

## Encryption

PVC archives use AGE.
Enable encryption by providing a public key:

```shell
kube-dump pvc save dir ./backup --recipient age1...
```

Encrypt an existing archive:

```shell
kube-dump pvc encrypt data.tar.zst data.tar.zst.age --recipient age1...
```

Decrypt an archive:

```shell
kube-dump pvc decrypt data.tar.zst.age data.tar.zst --identity ./age-key.txt
```

See [Encryption](encryption.md).

## Inspecting and extracting

Inspect local copies:

```shell
kube-dump pvc inspect dir ./backup
```

Inspect copies in S3:

```shell
kube-dump pvc inspect s3 --s3-uri s3://backup/prod/
```

Extract an archive into a directory:

```shell
kube-dump pvc extract ./data.tar.zst ./restored
```

kube-dump restores files but does not create a PVC automatically.

## Restoring into a PVC

Create the target PVC beforehand.
Source and target are specified separately,
so data can be restored into another namespace or PVC:

```shell
kube-dump pvc restore dir ./backup \
  --pvc production/postgres-data \
  --target-pvc recovery/postgres-data
```

> [!IMPORTANT]
> The target PVC must be `Bound`, use `Filesystem` mode, not be used by a Pod,
> and have capacity at least as large as the uncompressed archive.

For `merge`, this is only a minimum check:
used space inside the volume is not queried separately.
Before validation and writing,
kube-dump acquires a Kubernetes Lease for the target PVC.
This prevents two restores from writing to the same PVC at the same time,
but it does not stop workload Pods.
The PVC usage check is still performed separately.

The default `empty-only` policy refuses to write to a non-empty target PVC.
Choose `merge` or `replace` explicitly when needed:

```shell
kube-dump pvc restore dir ./backup \
  --pvc production/postgres-data \
  --target-pvc recovery/postgres-data \
  --existing merge
```

> [!CAUTION]
> `replace` removes existing entries
> at the root of the target PVC before extraction.
> A failure does not roll back data already written.

The latest copy is selected by default.
Select a specific revision with `--revision 20260904T010203Z`.

For S3, use the equivalent command:

```shell
kube-dump pvc restore s3 \
  --s3-uri s3://backup/prod/ \
  --pvc production/postgres-data \
  --target-pvc recovery/postgres-data
```

An encrypted copy requires `--identity` and is decrypted as a stream.
The command does not stop workloads.
Restore creates a temporary helper Pod and Kubernetes Lease;
S3 restore also creates temporary Secrets when needed,
then removes them after the operation.

The target is checked immediately
before the helper Pod is created as well as before restore.
The PVC UID must still match, and it must remain `Bound`,
filesystem-mode, large enough, and unused by a running Pod.

The archive validator rejects archives with more than 1,000,000 entries.
This is a safety bound for malformed archives, not a limit on the PVC size.
For a large failed backup, up to eight detailed PVC errors are retained
and the command reports how many additional details were omitted.
