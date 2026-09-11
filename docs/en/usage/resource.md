# Kubernetes resources

The `resource` command saves Kubernetes objects as YAML.

Before writing, kube-dump applies the profile.
The profile selects objects, removes runtime fields,
and protects values when encryption is enabled.

See the complete list of options
in the [CLI reference](../../cli.md#resource).

## Saving to a directory

```shell
kube-dump resource save dir ./backup
```

The `backup` profile is used by default.

Show the built-in profile:

```shell
kube-dump profile show backup
```

To save objects without the cleanup and restrictions
of the built-in profile, use `raw`:

```shell
kube-dump resource save dir ./backup --profile raw
```

Backups do not remove anything by default:
objects that disappeared from Kubernetes or are excluded
by a new profile remain in the destination.
Use `--prune` to remove files owned by the previous resource backup
but no longer selected:

```shell
kube-dump resource save dir ./backup --prune
```

For directory, Git, and S3 destinations, `--prune` runs only after a complete
collection and removes only paths from the destination ownership manifest.
Every complete save updates this manifest; without `--prune`,
it retains the history needed by a later prune.
For directory and Git backups it is stored at
`resources/.kube-dump/ownership.yaml`.
One destination represents one logical resource backup;
use separate directories or S3 prefixes for independent schedules.
Directory and Git trees are staged before publication.
S3 uploads a new generation before switching the current pointer.
If collection or publication fails, the existing destination remains unchanged.
Without `--strict`, collection continues to gather diagnostics after warnings;
with `--strict`, it stops at the first collection error.

> [!CAUTION]
> Keep arbitrary user files outside a backup tree.
> They are not part of the kube-dump format
> and are not managed by save, prune, or transform operations.

## Saving to Git

Git is convenient for YAML:
changes can be reviewed with the usual `git diff`,
and history is kept by Git itself:

```shell
kube-dump resource save git ./backup \
  --git-remote-url git@example.com:infra/kube-backup.git \
  --git-branch main --git-commit --git-push
```

Git is for resources only.
PVCs and container images are not saved there.

## Saving to S3

```shell
kube-dump resource save s3 --s3-uri s3://backup/prod/
```

For an S3-compatible service, specify its endpoint and region as needed.
See the [CLI reference](../../cli.md#resource-save-s3) for the exact flags.

## Archive

If you need one file instead of a YAML tree, save resources to an archive:

```shell
kube-dump resource save archive ./backup.tar.zst
```

You can write the resource archive directly to S3:

```shell
kube-dump resource save archive-s3 --s3-uri s3://backup/prod/resources.tar.zst
```

Gzip and Zstandard are supported.
The whole archive can be encrypted with AGE.

See [Encryption](encryption.md).

## Output without intermediate files

```shell
kube-dump resource save stdout --profile backup
```

For example, pass the result directly to `kubectl`:

```shell
kube-dump resource save stdout --profile backup | kubectl diff -f -
```

> [!TIP]
> stdout is reserved for command data. Logs are not mixed into it.

## One-off selection

For simple cases, you do not need to write a separate profile.

Save objects from one namespace:

```shell
kube-dump resource save dir ./backup --namespace production
```

Save only selected resource kinds:

```shell
kube-dump resource save dir ./backup \
  --resource deployments --resource configmaps
```

Select objects by label:

```shell
kube-dump resource save dir ./backup \
  --label-selector 'app.kubernetes.io/part-of=my-app'
```

> [!WARNING]
> A collection warning makes the save command fail without publishing
> the staged result. Add `--strict` when the collection should stop earlier
> instead of continuing to gather objects and diagnostics.

The result keeps at most eight warning details to avoid unbounded diagnostics.
The completion log still reports the exact warning count in `warnings`
and the number of omitted details in `warnings_omitted`.

The ownership manifest is limited to 1 MiB. An unusually large resource
selection fails instead of producing an unbounded pruning manifest.

The CLI reference contains the complete set of selection options.

## Profile rules

For example, save ConfigMaps except the automatically created
`kube-root-ca.crt`:

```yaml
apiVersion: kube-dump/v2
kind: Profile

metadata:
  name: production
  description: Production resource backup

resources:
  selection:
    resources:
      - core/v1/configmaps
      - '!core/v1/configmaps:kube-root-ca.crt'
```

The following rule removes runtime fields from selected resources:

```yaml
resources:
  rules:
    - name: remove-runtime-state
      match:
        resources:
          - '*'
      remove:
        - .metadata.uid
        - .metadata.resourceVersion
        - .metadata.managedFields
        - .status
```

See the [profile reference](../../profile.md) for the complete format.

## Encrypting individual fields

A profile can mark fields for encryption:

```yaml
resources:
  rules:
    - name: secret-data
      match:
        resources:
          - core/v1/secrets
      encrypt:
        - .data.*
        - .stringData.*
```

Keys and the encryption mode are supplied separately at runtime.

See [Encryption](encryption.md).

## Reading saved resources

`resource cat` combines saved objects into a YAML stream:

```shell
kube-dump resource cat dir ./backup |
  kubectl apply -f -
```

If saved fields are encrypted, provide the AGE private key:

```shell
kube-dump resource cat dir ./backup --identity ./age-key.txt |
  kubectl apply -f -
```

Equivalent commands are available for Git, S3, and archives.

kube-dump does not try to replace `kubectl`, Argo CD, or Flux.
Its job is to save data and return ordinary YAML.
