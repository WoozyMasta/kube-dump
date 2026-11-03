# Profiles

A profile describes which data kube-dump should save.

One file contains independent sections:

```yaml
resources:
  ...

pvc:
  ...

images:
  ...
```

The complete list of fields is described in the JSON Schema
and the [profile reference](../../profile.md).

## Built-in profiles

The distribution includes these profiles:

* `backup` - regular backup;
* `export` - more aggressively cleaned YAML;
* `raw` - minimal processing and broad resource coverage.

Show the contents of the `backup` profile:

```shell
kube-dump profile show backup
```

List available profiles:

```shell
kube-dump profile ls
```

## Custom profile

Start with a minimal profile:

```yaml
apiVersion: kube-dump/v2
kind: Profile

metadata:
  name: production
  description: Production backup profile

resources: {}
pvc: {}
images: {}
```

Validate the profile before starting a backup:

```shell
kube-dump profile validate ./production.yaml
```

Pass the profile to a save command:

```shell
kube-dump resource save dir ./backup --profile ./production.yaml
```

The same file can be passed to `pvc save` and `image save`.

## Resources

This profile saves selected resource kinds from the `production` namespace:

```yaml
resources:
  selection:
    resources:
      - core/v1/configmaps
      - core/v1/secrets
      - apps/v1/deployments
    namespaces:
      - production
```

Namespace, name, and resource expressions support a leading `!` for exclusion:

```yaml
resources:
  selection:
    resources:
      - core/v1/configmaps
      - '!core/v1/configmaps:kube-root-ca.crt'
```

> [!NOTE]
> Always quote values beginning with `!` in YAML.

Label and annotation selectors support
`In`, `NotIn`, `Exists`, and `DoesNotExist` operators.

## Removing empty values

To omit explicitly empty fields, enable `omitEmpty`:

```yaml
resources:
  omitEmpty: true
```

> [!IMPORTANT]
> Empty maps, lists, and `null` values are omitted.
> Zero values, `false`, and empty strings are preserved.

## Removing fields

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

## Encrypted fields

```yaml
resources:
  rules:
    - name: secret-data
      match:
        resources:
          - core/v1/secrets
      encrypt:
        - .data.*
```

The profile only marks fields.
The key and encryption mode are supplied as command-line flags.

See [Encryption](encryption.md).

## PVCs

```yaml
pvc:
  selection:
    namespaces:
      - production
    names:
      - 'postgres-*'
      - 'redis-*'
```

PVCs can also be selected by labels and annotations.

## Images

```yaml
images:
  selection:
    namespaces:
      - production
    references:
      - registry: ghcr.io
        repository: acme/*
        tags:
          - 'release-*'
```

You can also filter by Pod labels, annotations, container type, and Pod owner.

## Managing profiles

Use a profile directly as a file or install it into the profile directory:

```shell
kube-dump profile install ./production.yaml
```

After installation, refer to it by name:

```shell
kube-dump resource save dir ./backup --profile production
```

Main profile management commands:

```shell
kube-dump profile ls
kube-dump profile show production
kube-dump profile validate ./production.yaml
kube-dump profile edit production
kube-dump profile rm production
kube-dump profile which production
```

Copy a built-in profile as a starting point:

```shell
kube-dump profile cp builtin:backup production
```

For ongoing automation,
keep the profile in Git next to the rest of the infrastructure
and run `profile validate` before the backup.
