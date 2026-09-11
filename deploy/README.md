# Kubernetes deployment

This directory contains Kustomize components
for running kube-dump in Kubernetes.
Kustomize is native to `kubectl` and requires no separate tool such as Helm.

Create a small, user-owned Kustomization and choose the workload,
permissions, storage, and kube-dump command you need.

## Components

Use the common component in every composition:

* `deploy/` - image, labels, build metadata, and runtime ConfigMap.

Choose one workload:

* `resources/cronjob/` - scheduled CronJob;
* `resources/pod/` - one-shot Pod.

Choose one permission set:

* `components/rbac/resource/` - `resource save` and `resource export`;
* `components/rbac/image/` - `image save` and `image inspect`;
* `components/rbac/pvc/` - `pvc save` and `pvc restore`;
* `components/rbac/all/` - permissions for all three operation groups;
* `components/rbac/view/` - only the standard Kubernetes `view` role.

The RBAC components add the shared `ServiceAccount`
and keep its binding namespace aligned with the parent Kustomization.
The PVC RBAC component also grants access to `coordination.k8s.io/leases`
for serializing concurrent restores.
The `view` component is only a low-level building block;
it is not sufficient for all kube-dump commands.
The lower-level `components/serviceaccount/` component is available
for custom compositions and normally does not need to be selected directly.

Add local storage only when the destination is a filesystem path:

* `components/storage/volume/` - creates the backup PVC;
* `components/storage/cronjob/` - mounts it into a CronJob;
* `components/storage/pod/` - mounts it into a Pod.

Git and S3 destinations do not need these storage components.

Workload components do not set `args`; without a command patch,
the image runs its default `version` command.
Add the operation and its positional destination to a workload patch.
Pass changing options through `KUBE_DUMP_*` environment variables.
Storage components only add the backup PVC mount.

The workload runs as UID/GID `65532` with a read-only root filesystem
and the `RuntimeDefault` seccomp profile. `/tmp` is an `emptyDir`;
set `emptyDir.sizeLimit` in a local patch when a fixed limit is needed.
The `fsGroup` setting allows writing to the backup PVC.

PVC helper Pods are created separately when needed.
They run as root to preserve arbitrary file ownership and use the source
or target PVC, `/tmp`, and explicitly configured Secrets.
Set their resource policy with a namespace `LimitRange` or admission policy.

The common component provides a ConfigMap
and both workload templates import it with `envFrom`.
Extend it with a generator using the same name:

```yaml
configMapGenerator:
  - name: kube-dump
    behavior: merge
    literals:
      - KUBE_DUMP_NAMESPACE=production
      - KUBE_DUMP_PROFILE=backup
```

`merge` extends the values from the common component.
Use `replace` only when you want to provide the complete ConfigMap yourself.
Use `create` with another name for a separate ConfigMap;
it does not extend `kube-dump`.

The `--some-option` CLI form normally maps to `KUBE_DUMP_SOME_OPTION`.
Keep the operation and destination in `args`;
put namespaces, profiles, retry settings,
and similar tunables in the ConfigMap or Secret.

## Example: scheduled resource backup

Create these files in your deployment repository:

```text
my-kube-dump/
├── kustomization.yaml
└── patches/
    ├── resource-save-cronjob.yaml
    └── scratch-and-resources.yaml
```

`my-kube-dump/kustomization.yaml`:

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

patches:
  - path: patches/resource-save-cronjob.yaml
  - path: patches/scratch-and-resources.yaml
```

`my-kube-dump/patches/resource-save-cronjob.yaml`:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: kube-dump
spec:
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

Render the final manifest before applying it:

```shell
kubectl kustomize ./my-kube-dump > kube-dump.yaml
```

Create the namespace and apply the composition:

```shell
kubectl create namespace kube-dump
kubectl apply -k ./my-kube-dump
```

Check the workload or start it once manually:

```shell
kubectl get cronjob,pvc -n kube-dump
kubectl create job --from=cronjob/kube-dump kube-dump-manual -n kube-dump
kubectl logs -n kube-dump job/kube-dump-manual
```

## Scratch and helper resources

The workload's `/tmp` is an unbounded `emptyDir` by default.
Set a limit and resource requests in a small patch
when the deployment policy requires explicit values.

For example, save this as `patches/scratch-and-resources.yaml`:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: kube-dump
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: kube-dump
              resources:
                requests:
                  cpu: 500m
                  memory: 200Mi
                limits:
                  cpu: 1
                  memory: 512Mi
          volumes:
            - name: tmp
              emptyDir:
                sizeLimit: 4Gi
```

PVC helper Pods are created dynamically in the source namespace,
so configure their CPU and memory policy
there with a `LimitRange` or admission policy.

## Variants

For a one-shot run, use `resources/pod/`
and a Pod patch instead of the CronJob component and patch.

For S3 or Git, omit the storage component and change the command arguments.
Use `rbac/image`, `rbac/pvc`, or `rbac/all` according to the operation.

For a local checkout, replace the GitHub URLs with paths such as:

```text
../../deploy/resources/cronjob
../../deploy
../../deploy/components/rbac/resource
```

## Multiple compositions

The base components use `kube-dump` as the resource name.
If resource and PVC backups are deployed independently in the same namespace,
give each composition a different prefix:

```yaml
namePrefix: resources-
```

Use another prefix such as `pvc-` in the second composition.
Kustomize updates references to the ServiceAccount, ConfigMap, Secret, PVC,
and RBAC objects automatically.
An externally created Secret must use the resulting prefixed name.

The common ConfigMap disables the generator hash suffix,
so its name remains stable and it can be shared
when compositions intentionally use the same configuration.

## Secrets

The common component provides the runtime ConfigMap.
Add a Secret only when the selected command needs secret values.
For example, an encrypted operation
can receive an identity passphrase through an environment variable:

```shell
kubectl create secret generic kube-dump \
  --namespace kube-dump \
  --from-literal=KUBE_DUMP_IDENTITY_PASSPHRASE='replace-me'
```

Do not put real passphrases in shell history.
Use a file, an external secret manager,
or a CI secret store for production deployments.

Temporary PVC helper Pods are labelled with
`app.kubernetes.io/managed-by=kube-dump`, `kube-dump/source-namespace`,
`kube-dump/source-pvc`, and, when available, `kube-dump/run-id`.
They also have a 24-hour Kubernetes active deadline, so an abandoned helper
does not hold a PVC indefinitely.
After an interrupted run, inspect these labels and remove only confirmed
orphaned helpers:

```shell
kubectl get pods -n kube-dump -l app.kubernetes.io/managed-by=kube-dump \
  -L kube-dump/source-pvc,kube-dump/run-id
kubectl delete pod -n kube-dump <orphaned-helper-name>
```
