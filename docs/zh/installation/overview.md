# 安装与运行

kube-dump 以单个可执行文件和容器镜像的形式发布。
它可以手动运行，也可以从 CI 或 Kubernetes 中运行。

## 安装可执行文件

最新稳定版本提供以下预构建二进制文件：

 架构/操作系统 | Linux | macOS | Windows
------------ | ----- | ----- | --------
 amd64 | [Linux amd64] | [macOS amd64] | [Windows amd64]
 arm64 | [Linux arm64] | [macOS arm64] | [Windows arm64]

所有已发布版本都可以在 [GitHub Releases] 中找到。

使用 Go 1.27 或更高版本安装当前版本：

```shell
go install github.com/woozymasta/kube-dump/v2/cmd/kube-dump@latest
```

验证安装：

```shell
kube-dump version
```

## 在本地运行

默认情况下，kube-dump 使用 kubeconfig 和当前 Kubernetes context：

```shell
kube-dump resource save dir ./backup
```

也可以显式指定其他 kubeconfig 或 context：

```shell
kube-dump resource save dir ./backup \
  --kubeconfig ~/.kube/config --context production
```

在 Kubernetes 中安装请参阅[Kubernetes 部署](kubernetes.md)，从 CI 运行请参阅
[CI 集成](ci.md)。

## 容器镜像

镜像发布在以下仓库：

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1
docker.io/woozymasta/kube-dump:2.0.0-rc.1
```

带有 Shell 和调试、CI 脚本所需工具的 debug 版本单独发布：

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
docker.io/woozymasta/kube-dump:2.0.0-rc.1-debug
```

自动化任务应固定具体版本，不要使用浮动的 `latest` 标签。

主镜像很小，专用于运行 kube-dump。带有 `-debug` 后缀的镜像包含
CI 脚本和诊断所需的 Shell 及工具。

## 权限

Kubernetes 权限取决于所运行的命令和使用的配置文件。在 Kustomize 组合中选择
最小权限组件：`components/rbac/resource`、`components/rbac/image` 或
`components/rbac/pvc`。详情参阅[Kubernetes 部署](kubernetes.md)。

[Linux amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-linux-amd64
[Linux arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-linux-arm64
[macOS amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-darwin-amd64
[macOS arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-darwin-arm64
[Windows amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-windows-amd64.exe
[Windows arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-windows-arm64.exe
[GitHub Releases]: https://github.com/WoozyMasta/kube-dump/releases
