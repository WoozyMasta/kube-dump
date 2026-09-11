<!-- markdownlint-disable MD033 MD041 -->
<div align="right">
  <p>
    <a href="./README.md">English</a> ·
    <a href="./README.ru.md">Русский</a> ·
    <a href="./README.zh.md">中文</a>
  </p>
</div>
<div align="center">
  <img src="./docs/assets/images/logo-wide.png" alt="kube-dump" width="600">
  <p>
    <a href="https://kube-dump.woozymasta.ru/zh/">文档网站</a> ·
    <a href="./deploy/README.md">在 Kubernetes 中运行</a> ·
    <a href="./docs/README.md">项目文档</a>
  </p>
</div>
<!-- markdownlint-enable MD033 -->

# kube-dump

**kube-dump** 是一个用于保存 Kubernetes 环境实际状态的工具，适合在
需要固定当前状态时使用。

在测试 namespace、开发者沙箱、临时预发布环境或演示环境中，状态经常
通过手动操作发生变化。有人更新 Deployment、创建 ConfigMap、试用新的
operator、向 PVC 写入数据，或者运行临时镜像。即使环境后来发生变化或
被删除，这些状态也可能在一段时间后再次需要。

这类环境不一定具备 GitOps、独立的备份系统或严格的生命周期。这并没有
问题，但重要状态可能因此没有可移植的副本。

kube-dump 将这些状态保存为易读格式，可以存储在本地、Git 或兼容 S3 的
存储中，并用于恢复。

## 安装

最新稳定版本提供了以下文件：

操作系统 / 架构 | Linux | macOS | Windows
---------------- | ------------- | ------------- | ---------------
amd64 | [Linux amd64] | [macOS amd64] | [Windows amd64]
arm64 | [Linux arm64] | [macOS arm64] | [Windows arm64]

所有已发布版本都可以在 [GitHub Releases][ghr] 页面找到。

如果已安装 Go 1.27 或更高版本，可以执行以下命令安装当前版本：

```shell
go install github.com/woozymasta/kube-dump/v2/cmd/kube-dump@v2.0.0-rc.1
```

如果需要在 Kubernetes 中运行，可以使用现成的
[Kustomize 组件][kubernetes]。

容器镜像发布在以下仓库：

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1
docker.io/woozymasta/kube-dump:2.0.0-rc.1
```

对于 CI、调试和脚本，可以使用包含 shell 及其他工具的镜像：

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
docker.io/woozymasta/kube-dump:2.0.0-rc.1-debug
```

更多安装方式请参阅[文档][docs]。

## 保存的内容

kube-dump 保存 Kubernetes 状态中的三个独立部分：

* Kubernetes 资源 - 可存储在 Git、S3 或归档中的 YAML 清单；
* PVC 数据 - 可以从 CSI 快照读取的文件归档；
* 容器镜像 - 标准 OCI Image Layout，包括临时镜像和合并请求构建的镜像。

每一类数据都可以单独保存，也可以一起保存。

## 使用方式

每一类数据都有对应的命令。例如，将资源保存到本地：

```sh
kube-dump resource save dir ./backup
```

或者发送到兼容 S3 的存储：

```sh
kube-dump resource save s3 --s3-uri s3://backup/my-cluster/
```

PVC 数据和容器镜像分别使用 `pvc` 和 `image` 命令保存。

可以在维护或实验之前、清理临时镜像仓库之前，或者删除测试环境之前保存
当前状态。

## 它不替代什么

kube-dump 不替代 Argo CD、Flux、Velero、数据库 operator 或应用自身的
备份机制。

如果应用能够创建正确的逻辑数据库备份，应继续使用该机制。如果基础设施
已经完整地声明在 Git 中，Git 仍然是期望状态的来源。

kube-dump 的任务是以易读且可移植的形式记录集群在某一时刻实际存在的内容，
为后续恢复提供依据。

## 配置文件与数据保护

配置文件定义资源、PVC 和镜像的选择与处理方式。对于 Kubernetes 清单，
配置文件还定义清理规则以及需要加密的字段。

内置配置文件：

* [backup] - 用于备份配置、访问控制和常见 operator 对象，并删除服务器
  生成的状态；
* [export] - 用于生成干净、可移植的清单，并删除运行时元数据和空字段；
* [raw] - 不进行清理或过滤，保存所有发现的对象。

YAML 中的敏感值可以使用 age 或确定性的 AES-256-SIV 加密。资源和 PVC
归档还可以额外使用 age 保护。

## 更多信息

此 README 不是完整文档。安装、命令、配置文件、加密、PVC 和镜像的详细
说明请参阅[文档][docs]。

<!-- links -->

[docs]: https://kube-dump.woozymasta.ru/zh/
[ghr]: https://github.com/WoozyMasta/kube-dump/releases
[kubernetes]: ./deploy/README.md
[backup]: ./internal/profile/builtin/backup.yaml
[export]: ./internal/profile/builtin/export.yaml
[raw]: ./internal/profile/builtin/raw.yaml

[Linux amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-linux-amd64
[Linux arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-linux-arm64
[macOS amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-darwin-amd64
[macOS arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-darwin-arm64
[Windows amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-windows-amd64.exe
[Windows arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-windows-arm64.exe
