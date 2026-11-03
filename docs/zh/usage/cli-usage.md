# CLI 使用

本页介绍 `kube-dump` 命令行的常用规则。完整的命令和选项列表请参阅
[CLI 参考](../../cli.md)。

## 帮助

使用 `--help` 或其短选项 `-h` 请求帮助：

```shell
kube-dump --help
kube-dump resource -h
kube-dump resource save -h
```

也可以使用单独的 `help` 命令，并将命令路径作为独立参数传入：

```shell
kube-dump help
kube-dump help resource
kube-dump help resource save
```

## 环境变量

CLI 使用 `KUBE_DUMP_` 作为环境变量前缀。变量名根据长选项生成：转换为大写，
并将每个 `-` 替换为 `_`：

```text
--profile         -> KUBE_DUMP_PROFILE
--retry-attempts  -> KUBE_DUMP_RETRY_ATTEMPTS
--no-progress     -> KUBE_DUMP_NO_PROGRESS
```

CLI 参考会在相应选项旁列出可使用的环境变量。

显式传入的选项优先于环境变量。默认会加载 `.env` 文件。
使用 `--env-file PATH` 选择其他文件，使用 `--no-env` 禁用加载，使用
`--env-override` 允许 `.env` 中的值替换进程中已有的变量。

## 环境变量中的重复选项

重复选项可以传入多次：

```shell
kube-dump resource save stdout \
  --namespace production \
  --namespace staging \
  --resource apps/v1/deployments \
  --resource core/v1/configmaps
```

对于重复选项，环境变量中使用逗号分隔：

```shell
export KUBE_DUMP_NAMESPACE='production,staging'
```

PowerShell：

```powershell
$env:KUBE_DUMP_NAMESPACE = 'production,staging'
```

重复的参数（例如 `--git-push-option`）也使用逗号作为分隔符。

一些选项接受 `key=value` 格式的键值对列表。对于 `--registry-mirror`，
`=` 分隔键和值，逗号分隔多条记录：

```shell
export KUBE_DUMP_REGISTRY_MIRROR='registry.example.com=mirror.example.com,ghcr.io=mirror.example.net'
```

如果值本身包含逗号，请使用多个选项传入，而不要放在同一个环境变量中。

## Shell 补全

内置的 `completion` 命令可以为 Bash、Zsh 和 PowerShell 生成补全脚本。
如果未指定输出路径，脚本会打印到 stdout。

Bash：

```shell
. <(kube-dump completion --shell bash)
```

要永久启用 Bash 补全：

```shell
mkdir -p ~/.local/share/bash-completion/completions
kube-dump completion --shell bash \
  ~/.local/share/bash-completion/completions/kube-dump
```

Zsh：

```shell
kube-dump completion --shell zsh > ~/.zfunc/_kube_dump
```

PowerShell：

```powershell
$path = Join-Path $HOME 'Documents/PowerShell/Completions/kube-dump.ps1'
New-Item -ItemType Directory -Force (Split-Path $path) | Out-Null
kube-dump completion --shell pwsh $path
. $path
```

## 其他文档格式

CLI 可以将命令说明写入多种格式：

```shell
kube-dump docs md docs/cli.md --program-name kube-dump --style posix --toc
kube-dump docs html docs/cli.html --program-name kube-dump --style posix
kube-dump docs man kube-dump.1 --program-name kube-dump --style posix
kube-dump docs json docs/cli.json
```

如果未指定输出路径，结果会打印到 stdout。

## 内部 `volume` 命令

`volume` 用于在临时 Pod 中读取、写入和传输 PVC 数据。
查看帮助：

```shell
kube-dump help volume
```

## 配置文件 JSON Schema

Profile schema 已嵌入二进制文件，无需网络即可输出：

```shell
kube-dump profile schema > profile.schema.json
```

要在 YAML 中使用 schema 校验，请在 VS Code 中安装
[`redhat.vscode-yaml`](https://marketplace.visualstudio.com/items?itemName=redhat.vscode-yaml)
扩展，并在配置文件开头添加：

```yaml
# yaml-language-server: $schema=./profile.schema.json
apiVersion: kube-dump/v2
kind: Profile
```

也可以直接引用仓库中的 schema：

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/WoozyMasta/kube-dump/HEAD/pkg/profile/schema/profile.schema.json
```

为了保证可复现的校验，请将 URL 固定到具体发布版本，而不要使用 `HEAD`。

在开始备份前，可以在 CI 中校验配置文件：

```shell
kube-dump profile validate ./profile.yaml
```
