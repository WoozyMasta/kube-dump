# Recovery

kube-dump is not a complete Kubernetes recovery system.
It saves data in standard formats and lets you read it back:

* resources - YAML;
* images - OCI Image Layout;
* PVCs - compressed filesystem archives.

The remaining recovery work uses ordinary Kubernetes,
registry, and application tools.
This keeps recovery explicit:
kube-dump does not hide PVC creation, image changes,
workload startup, or data migration behind one command.

## Start here

Inspect the saved data first:

```shell
kube-dump resource inspect dir ./backup
kube-dump pvc inspect dir ./backup
kube-dump image inspect dir ./backup
```

Use the corresponding `inspect s3` commands for an S3 copy.
Prepare the needed AGE keys before reading encrypted data.

## Format compatibility

Saved data includes the metadata required to validate and read it:
PVC archives use `metadata.yaml`, images use the standard OCI Image Layout,
and resource backups use an ownership manifest.
Keep these files with the saved data and use a matching kube-dump v2 release
when moving or restoring it.

## Typical recovery order

For a complete environment, this order is usually useful:

1. prepare the new cluster and storage;
1. publish the saved container images;
1. prepare the resource YAML;
1. create PVCs without starting applications that immediately use them;
1. restore PVC data;
1. apply or enable workloads;
1. verify the applications.

This order is not mandatory for every cluster, but the main principle is:
restore dependencies and data before starting applications.

## Kubernetes resources

Read saved resources as a YAML stream:

```shell
kube-dump resource cat dir ./backup > resources.yaml
```

If fields are encrypted:

```shell
kube-dump resource cat dir ./backup \
  --identity ./age-identity.txt > resources.yaml
```

Review the result and apply it:

```shell
kubectl diff -f resources.yaml
kubectl apply -f resources.yaml
```

For serious recovery, decide which objects must be created first:
namespaces, CRDs and operators, RBAC, configuration, PVCs, workloads,
and then Services, Ingresses, and other application objects.
Custom resources cannot be applied until their CRD or operator exists.

## Do not start applications before restoring data

> [!WARNING]
> Do not immediately apply all Deployments and StatefulSets
> while their PVCs are empty.
> An application may create a new empty database, run migrations,
> change the directory layout, or write new data before the old data returns.

Usually create PVCs and base resources, restore data,
and only then start the workloads.

## Container images

If the original registry is unavailable,
publish the saved images to a registry that is available during recovery:

```shell
kube-dump image push dir ./backup registry.example.com/recovered
kube-dump image push s3 registry.example.com/recovered --s3-uri s3://backup/prod/
```

Then make the restored Pods use the available image names.
Either restore the old registry address through DNS, a mirror,
or container runtime settings, or update YAML with Kustomize,
Helm values, GitOps tooling, or another YAML tool.

kube-dump deliberately does not introduce a custom image-rewrite language.
For complex registry work, use [ORAS], [crane], or [regctl].

## PVCs

kube-dump restores a selected copy into an existing PVC;
it does not create a new PVC automatically:

```shell
kube-dump pvc restore dir ./backup \
  --pvc production/postgres-data \
  --target-pvc recovery/postgres-data

kube-dump pvc restore s3 \
  --s3-uri s3://backup/prod/ \
  --pvc production/postgres-data \
  --target-pvc recovery/postgres-data
```

The default requires an empty target PVC.
Choose `merge` or `replace` through `--existing`;
use `--revision` to select a particular copy.
The target must be Bound, use `Filesystem` mode,
be large enough for the uncompressed data,
and not be mounted by a running Pod.

> [!CAUTION]
> `replace` removes existing target contents before writing.
> A stream failure does not roll back a partial result.

For a manual transfer, extract the archive
and copy the files into a temporary Pod that mounts the target PVC.
This is suitable for small and medium volumes;
for large volumes use a storage-native or in-cluster transfer path.

For example, create a temporary Pod with the target PVC mounted:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pvc-restore
spec:
  restartPolicy: Never
  containers:
    - name: restore
      image: alpine:3.22
      command: [sh, -c, sleep infinity]
      volumeMounts:
        - name: data
          mountPath: /restore
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: postgres-data
```

Apply it and stream extracted files into the mounted volume:

```shell
kubectl apply -f restore-pod.yaml
tar -C ./restored -cf - . |
  kubectl exec -i pvc-restore -- tar -C /restore -xf -
kubectl delete pod pvc-restore
```

This simple method can be slow for very large volumes
because the stream passes through the Kubernetes API.

## Large PVCs

For hundreds of gigabytes or terabytes,
prefer a path that does not send the whole stream through the Kubernetes API:
a temporary Pod or Job that downloads from S3, storage-native copy tools,
CSI snapshots and clones, `rsync` inside the cluster,
or an application-provided import mechanism.

kube-dump remains the portable source copy;
the transfer method should match the storage system.

## Databases and file ownership

A PVC archive is a filesystem copy, not necessarily a logical database backup.
For PostgreSQL, MySQL, ClickHouse, Elasticsearch, and similar systems,
follow the application's own recovery rules.
Tools such as `pg_dump`, `pg_restore`, `mysqldump`, `xtrabackup`,
or an application API may be preferable for the primary database backup.

Preserve permissions, owners, groups, symbolic links, and directory structure.
The temporary Pod must be allowed to restore the required owners and modes.

## Other StorageClass and cluster migration

The new cluster does not need the same StorageClass.
Create a new PVC of the required size,
wait until it is ready, and restore the files into it.

For a migration, save all three data types to a shared destination:

```shell
kube-dump resource save s3 --s3-uri s3://backup/migration/
kube-dump pvc save s3 --s3-uri s3://backup/migration/
kube-dump image save s3 --s3-uri s3://backup/migration/
```

Restore them as:

```text
images -> new registry
resources -> YAML
PVCs -> new volumes
```

> [!TIP]
> Exercise the complete process once
> in a test environment before a real migration.

## GitOps

If Argo CD, Flux, or another GitOps tool manages the resources,
do not apply the saved YAML over it without a reason.
Git remains the desired-state source, while kube-dump preserves observed state,
PVC data, and images that may vanish from a registry.

## What kube-dump intentionally does not do

kube-dump does not automatically:

* build a dependency graph for every object;
* determine the installation order of operators and CRDs;
* create new PVCs and populate them;
* rewrite every container image reference;
* recover external cloud services;
* run database-specific migrations;
* guarantee application readiness after `kubectl apply`.

Those tasks depend on the cluster and application.
kube-dump preserves the configuration, data,
and images needed for recovery in standard formats.

## Test recovery regularly

The best way to check a backup is to restore it periodically
in an isolated environment.
A minimal drill uses a new test cluster,
an empty registry or separate registry prefix,
empty PVCs, saved images, PVC data, and resources.

This reveals external DNS records, cloud secrets, certificates,
StorageClass dependencies, external databases,
inaccessible registries, and undocumented manual actions.

[ORAS]: https://github.com/oras-project/oras
[crane]: https://github.com/google/go-containerregistry
[regctl]: https://github.com/regclient/regctl
