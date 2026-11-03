# kube-dump

![kube-dump 徽标](../assets/images/logo-wide.png)

kube-dump 保存 Kubernetes 状态的三个部分：

* Kubernetes 资源 YAML；
* PVC 数据；
* Pod 使用的容器镜像。

这三部分相互独立。你可以只保存 YAML、只保存卷，或者只保存镜像。
完整的环境备份通常需要保存全部三部分。

完整的命令和选项列表请参阅 [CLI 参考](../cli.md)，所有配置文件字段请参阅
[配置文件参考](../profile.md)。环境变量、Shell 补全和文档生成的实际用法请参阅
[CLI 使用](usage/cli-usage.md)。

首次运行前，请先阅读[安装与运行](installation/overview.md)。

## 快速开始

将资源保存到本地目录：

```shell
kube-dump resource save dir ./backup
```

将 PVC 数据保存到本地目录：

```shell
kube-dump pvc save dir ./backup
```

将正在使用的容器镜像保存到本地目录：

```shell
kube-dump image save dir ./backup
```

同样的命令也可以保存到 S3；只需更换后端和目标路径：

```shell
kube-dump resource save s3 --s3-uri s3://backup/prod/
kube-dump pvc save s3 --s3-uri s3://backup/prod/
kube-dump image save s3 --s3-uri s3://backup/prod/
```

三种数据都可以使用同一个本地目录或 S3 前缀。

## 资源

`resource` 读取 Kubernetes 对象，应用配置文件，然后保存 YAML：

```shell
kube-dump resource save dir ./backup --profile backup
```

保存的 YAML 可以直接通过管道交给 `kubectl`：

```shell
kube-dump resource cat dir ./backup | kubectl apply -f -
```

参阅[Kubernetes 资源](usage/resource.md)。

## PVC

`pvc` 将每个选定 PVC 的文件系统保存为单独的归档：

```shell
kube-dump pvc save dir ./backup
```

默认情况下，kube-dump 使用卷快照并读取快照副本，而不是直接访问应用正在使用的
PVC。

参阅[PVC 数据](usage/pvc.md)。

## 镜像

`image` 检查正在运行的 Pod，并保存它们使用的容器镜像：

```shell
kube-dump image save dir ./backup
```

镜像使用标准 OCI Image Layout 保存，不采用 kube-dump 专用格式。

参阅[容器镜像](usage/image.md)。

## 配置文件

配置文件定义资源、PVC 和镜像的规则：

```shell
kube-dump profile show backup
```

你可以使用内置配置文件、Git 中的文件，或已安装到本地的配置文件。

参阅[配置文件](usage/profiles.md)。

## 加密

可以使用 AGE 或 AES-SIV 加密单独的 YAML 字段。

资源和 PVC 归档使用 AGE 加密。

参阅[加密](usage/encryption.md)。

## 运行 kube-dump

kube-dump 可以手动运行，也可以从 CI 或 Kubernetes 中按计划运行。

参阅[安装与运行](installation/overview.md)。
