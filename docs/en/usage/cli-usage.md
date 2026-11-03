# CLI usage

This page describes common `kube-dump` command-line rules.
The complete list of commands and flags is available
in the [CLI reference](../../cli.md).

## Help

Request help with `--help` or its short form `-h`:

```shell
kube-dump --help
kube-dump resource -h
kube-dump resource save -h
```

There is also a separate `help` command,
which accepts the command path as individual arguments:

```shell
kube-dump help
kube-dump help resource
kube-dump help resource save
```

## Environment variables

The CLI uses the `KUBE_DUMP_` prefix for environment variables.
A variable name is built from the long flag name:
it is converted to uppercase and each `-` is replaced with `_`:

```text
--profile         -> KUBE_DUMP_PROFILE
--retry-attempts  -> KUBE_DUMP_RETRY_ATTEMPTS
--no-progress     -> KUBE_DUMP_NO_PROGRESS
```

The CLI reference lists the environment variable next to the corresponding flag.

An explicitly supplied flag takes precedence over an environment variable.
The `.env` file is loaded by default.
Use `--env-file PATH` to select another file, `--no-env` to disable loading it,
and `--env-override` to let `.env` values replace variables
already present in the process environment.

## Repeated flags in environment variables

A repeated flag can be supplied more than once:

```shell
kube-dump resource save stdout \
  --namespace production \
  --namespace staging \
  --resource apps/v1/deployments \
  --resource core/v1/configmaps
```

For repeated flags, kube-dump uses a comma in the environment variable:

```shell
export KUBE_DUMP_NAMESPACE='production,staging'
```

In PowerShell:

```powershell
$env:KUBE_DUMP_NAMESPACE = 'production,staging'
```

The same comma separator is used for repeated options
such as `--git-push-option`.

Some flags accept a list of `key=value` pairs.
For `--registry-mirror`, `=` separates the key and value,
while records are separated by commas:

```shell
export KUBE_DUMP_REGISTRY_MIRROR='registry.example.com=mirror.example.com,ghcr.io=mirror.example.net'
```

If a value contains a comma, pass it as separate flags
instead of putting it in one environment variable.

## Shell completion

The built-in `completion` command generates completion
for Bash, Zsh, and PowerShell.
If no output path is provided, the script is printed to stdout.

Bash:

```shell
. <(kube-dump completion --shell bash)
```

To keep Bash completion enabled:

```shell
mkdir -p ~/.local/share/bash-completion/completions
kube-dump completion --shell bash \
  ~/.local/share/bash-completion/completions/kube-dump
```

Zsh:

```shell
kube-dump completion --shell zsh > ~/.zfunc/_kube_dump
```

PowerShell:

```powershell
$path = Join-Path $HOME 'Documents/PowerShell/Completions/kube-dump.ps1'
New-Item -ItemType Directory -Force (Split-Path $path) | Out-Null
kube-dump completion --shell pwsh $path
. $path
```

## Other documentation formats

The CLI can write command descriptions in several formats:

```shell
kube-dump docs md docs/cli.md --program-name kube-dump --style posix --toc
kube-dump docs html docs/cli.html --program-name kube-dump --style posix
kube-dump docs man kube-dump.1 --program-name kube-dump --style posix
kube-dump docs json docs/cli.json
```

If no output path is provided, the result is printed to stdout.

## Internal `volume` command

`volume` is used inside temporary Pods to read, write, and transfer PVC data.
For help:

```shell
kube-dump help volume
```

## Profile JSON Schema

The profile schema is embedded in the binary
and can be printed without network access:

```shell
kube-dump profile schema > profile.schema.json
```

To validate YAML against the schema, install the
[`redhat.vscode-yaml`](https://marketplace.visualstudio.com/items?itemName=redhat.vscode-yaml)
extension in VS Code and add this at the beginning of the profile:

```yaml
# yaml-language-server: $schema=./profile.schema.json
apiVersion: kube-dump/v2
kind: Profile
```

You can also reference the schema in the repository:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/WoozyMasta/kube-dump/HEAD/pkg/profile/schema/profile.schema.json
```

For reproducible validation, pin the URL to a release tag instead of `HEAD`.

Validate the profile in CI before starting a backup:

```shell
kube-dump profile validate ./profile.yaml
```
