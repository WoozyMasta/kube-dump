# Kubernetes deployment

The repository provides Kustomize components
for running `kube-dump` in a Kubernetes cluster.
Add them to your composition and configure the operation you need.
Kustomize is built into `kubectl`; no Helm installation is required.

The manifests do not choose an operation for you. Build a composition from:

1. a workload - `CronJob` or one-shot `Pod`;
1. RBAC for `resource`, `image`, or `pvc`;
1. the common component with the image and environment settings;
1. local storage, only when a `dir` destination needs it.

Git and S3 do not require a local PVC. For `dir`, use a container directory;
attach the `storage` component when data must survive between runs.

## Requirements

You need:

* `kubectl` with `kubectl apply -k` support;
* permission to create the selected workload and RBAC objects;
* a published kube-dump image accessible by cluster nodes;
* a StorageClass when local storage is used;
* CSI snapshot CRDs, a snapshot controller,
  and a compatible storage driver for `pvc save` with `snapshot-copy`.

The RBAC components create cluster roles and ClusterRoleBindings,
so applying them usually requires cluster administrator permissions.
The container itself runs as the `kube-dump` ServiceAccount.

## Components

### Common component

Add the repository's common component to every composition.
It overrides the image, adds standard labels, adds a base ConfigMap,
and disables interactive progress suitable for Pod logs.
It does not create a namespace, workload, ServiceAccount, permissions, or PVC.
Set the namespace in your own `kustomization.yaml`.

### Workload

Choose one:

* `resources/cronjob/` - scheduled execution with `concurrencyPolicy: Forbid`;
* `resources/pod/` - one-shot Pod.

The templates contain no `args`.
Without customization, the image runs its default `version` command.
Set the operation and its parameters in your composition.

### Permissions

Choose one RBAC component:

* `components/rbac/resource/` - `resource save`;
* `components/rbac/image/` - `image save` and `image inspect`;
* `components/rbac/pvc/` - `pvc save` and `pvc restore`;
* `components/rbac/all/` - all three groups.

The PVC component also grants access to `coordination.k8s.io/leases`,
which is required to serialize concurrent restores into the same PVC.

`components/rbac/view/` is an additional building block for custom compositions.
The standard Kubernetes `view` role is not sufficient for every operation.

### Local storage

Storage components are only needed for `dir`:

* `components/storage/volume/` creates PVC `kube-dump`;
* `components/storage/cronjob/` mounts it into a CronJob;
* `components/storage/pod/` mounts it into a Pod.

The workload storage components already include `storage/volume`,
so do not add all three.
The default PVC size is `1Gi`;
if that is not enough, override it with a patch.

## Example: resources to a local PVC

The following creates a nightly CronJob that saves resources
from the built-in `backup` profile to `/data/backup`.

Create this structure in your repository:

```text
kdump/
├── kustomization.yaml
└── patches/
    └── resource-save-cronjob.yaml
```

`kdump/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: kube-dump

resources:
  - https://github.com/WoozyMasta/kube-dump//deploy/resources/cronjob?ref=v2.0.0-rc.1

components:
  - https://github.com/WoozyMasta/kube-dump//deploy?ref=v2.0.0-rc.1
  - https://github.com/WoozyMasta/kube-dump//deploy/components/rbac/resource?ref=v2.0.0-rc.1
  - https://github.com/WoozyMasta/kube-dump//deploy/components/storage/cronjob?ref=v2.0.0-rc.1

configMapGenerator:
  - name: kube-dump
    behavior: merge
    literals:
      - KUBE_DUMP_NAMESPACE=production
      - KUBE_DUMP_PROFILE=backup

patches:
  - path: patches/resource-save-cronjob.yaml
```

`kdump/patches/resource-save-cronjob.yaml`:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: kube-dump
spec:
  schedule: "0 1 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: kube-dump
              args:
                - resource
                - save
                - dir
                - /data/backup
```

## Configuration and Secrets

The common component connects ConfigMap and Secret through `envFrom`.
Put the operation and destination in `args`,
and configure changing options through environment variables.
For `--some-option`, use `KUBE_DUMP_SOME_OPTION`.

The example's `configMapGenerator` extends the common ConfigMap.
`merge` keeps base values and overrides keys with the same name.
`replace` replaces the entire ConfigMap, so all required values must be listed.
`create` creates a different ConfigMap
and is not suitable for extending `kube-dump`.

Do not commit secret values to Kustomize files. Create a Secret separately:

```shell
kubectl create secret generic kube-dump \
  --namespace kube-dump \
  --from-literal=KUBE_DUMP_IDENTITY_PASSPHRASE='replace-me'
```

Use a Secret Manager or CI secret for the real value
rather than a literal in a command that is saved in shell history.
Mount file-based keys and profiles separately;
the environment variable then contains the mounted file path.

The `?ref=v2.0.0-rc.1` references pin the Kustomize manifests.
Change them to the desired release tag when upgrading kube-dump.

## Verify and apply

Render the manifests first:

```shell
kubectl kustomize ./kdump > kube-dump.yaml
```

Create the namespace and apply the composition:

```shell
kubectl create namespace kube-dump
kubectl apply -k ./kdump
```

Check the CronJob and PVC:

```shell
kubectl get cronjob,pvc -n kube-dump
kubectl describe cronjob kube-dump -n kube-dump
```

Run the same scenario immediately with a Job:

```shell
kubectl create job --from=cronjob/kube-dump kube-dump-manual -n kube-dump
kubectl logs -f -n kube-dump job/kube-dump-manual
```

## One-shot Pod

For a one-time operation, use `resources/pod/` and patch a `Pod`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: kube-dump

resources:
  - https://github.com/WoozyMasta/kube-dump//deploy/resources/pod?ref=v2.0.0-rc.1

components:
  - https://github.com/WoozyMasta/kube-dump//deploy?ref=v2.0.0-rc.1
  - https://github.com/WoozyMasta/kube-dump//deploy/components/rbac/resource?ref=v2.0.0-rc.1
  - https://github.com/WoozyMasta/kube-dump//deploy/components/storage/pod?ref=v2.0.0-rc.1

patches:
  - path: patches/resource-save-pod.yaml
```

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: kube-dump
spec:
  containers:
    - name: kube-dump
      args:
        - resource
        - save
        - dir
        - /data/backup
```

Apply and inspect the Pod:

```shell
kubectl apply -k ./kdump
kubectl logs -f -n kube-dump pod/kube-dump
kubectl get pod -n kube-dump
```

## Other operations and destinations

Workload and RBAC are independent of the destination.
Change the workload patch and environment configuration.
Keep only the operation and positional destination paths in `args`;
provide profile, namespace, PVC strategy, retry,
and other options through ConfigMap or Secret.

For images:

```yaml
args:
  - image
  - save
  - dir
  - /data/backup
```

The profile must contain image selection rules.
Images are stored under `images` in the selected destination.

For PVCs:

```yaml
args:
  - pvc
  - save
  - dir
  - /data/backup
```

Add `KUBE_DUMP_NAMESPACE`
and `KUBE_DUMP_STRATEGY=snapshot-copy` to the `configMapGenerator`.
`snapshot-copy` creates temporary snapshots, a cloned PVC, and a helper Pod.
The cluster must support CSI snapshots
and the ServiceAccount must have the `rbac/pvc` permissions.
On clusters without snapshots, use `pod` when direct reads are safe.

For Git and S3, do not add local storage.
Example S3 arguments:

```yaml
args:
  - resource
  - save
  - s3
```

Set the S3 URI through `KUBE_DUMP_S3_URI`, for example in `configMapGenerator`:

```yaml
literals:
  - KUBE_DUMP_S3_URI=s3://backup/production/resources
```

Pass Git and S3 credentials through environment variables or a Secret.
See the [CLI reference](../../cli.md).
PVC S3 backups use the separate `--s3-pod-endpoint`,
which must be reachable from the helper Pod.

## Namespace and local manifests

Change `namespace` in your `kustomization.yaml` to use another namespace.
Kustomize updates ServiceAccount and ClusterRoleBinding references;
manual replacements are not needed.

To inspect or modify the manifests locally,
clone the repository and replace the remote references with:

```text
../../deploy/resources/cronjob
../../deploy
../../deploy/components/rbac/resource
../../deploy/components/storage/cronjob
```

## Multiple compositions in one namespace

All base components use the name `kube-dump`,
so independent resource and PVC compositions conflict without customization.
Add different prefixes:

```yaml
namePrefix: resources-
```

and for the other composition:

```yaml
namePrefix: pvc-
```

Kustomize updates references to
ServiceAccount, ConfigMap, Secret, PVC, and RBAC objects.
An externally created Secret must use the prefixed name too.

The common ConfigMap has `disableNameSuffixHash: true`,
so its name is stable where compositions intentionally share the same settings.

Do not add multiple workload or RBAC components
unless you are deliberately building a custom composition;
they contain objects with the same names.

## Troubleshooting

Start with the rendered manifest and workload state:

```shell
kubectl kustomize ./kdump
kubectl get pods,jobs -n kube-dump
kubectl describe pod -n kube-dump -l app.kubernetes.io/name=kube-dump
kubectl logs -n kube-dump -l app.kubernetes.io/name=kube-dump --all-containers
```

Check the ServiceAccount permissions without running a backup:

```shell
kubectl auth can-i --list --as=system:serviceaccount:kube-dump:kube-dump
```

For `pvc save`, also check `VolumeSnapshotClass`, temporary snapshot state,
and helper image availability in the cluster registry.
