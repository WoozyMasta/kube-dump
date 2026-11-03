# 容器镜像

`image` 命令保存 Pod 使用的镜像。

它与 YAML 和 PVC 数据互为补充。清单中可能保留了标签，但镜像可能从仓库中消失，
或者标签之后指向不同内容。

完整选项列表请参阅 [CLI 参考](../../cli.md#image)。

## 保存

将镜像保存到本地目录：

```shell
kube-dump image save dir ./backup
```

将镜像保存到 S3：

```shell
kube-dump image save s3 --s3-uri s3://backup/prod/
```

镜像使用标准 OCI Image Layout 保存：

```text
images/
  oci-layout
  index.json
  blobs/
```

kube-dump 不引入自定义镜像格式。

再次保存到同一个 S3 前缀时，已有 blob 会按 digest 复用。
只有在捕获成功完成后才会发布 `index.json`。
`image download s3` 和 `image push s3` 只读取从该索引可达的内容图，
不会下载旧的孤立 blob。S3 索引通过条件更新发布；并发捕获不会静默覆盖
彼此的索引，而是由冲突结束。

大于 16 MiB 或包含超过 100,000 个顶层 manifest 的镜像索引会被拒绝。
这是为了限制检查和发布时的元数据规模。
大规模捕获失败时，命令最多保留八个镜像的详细错误，并报告被省略的详细错误数量；
失败计数仍包括所有镜像。

## 镜像选择方式

kube-dump 从以下字段获取镜像引用：

```text
spec.containers[].image
status.containerStatuses[].imageID
```

init container 中的相同字段也会读取。

`spec.containers[].image` 保留原始名称，例如：

```text
ghcr.io/acme/api:mr-123
```

如果可用，`imageID` 允许 kube-dump 使用实际运行的 digest。

保存的 OCI layout 会分别保留 `spec.containers[].image` 中声明的源引用
和运行中镜像的 digest。两者可能不同：前者描述 Pod 的声明，
后者标识实际运行的内容。

> [!WARNING]
> 如果无法使用准确的 digest，kube-dump 会回退到原始 `spec` 引用并写入警告。
> 此时可变标签可能已经指向另一个镜像。

## 命名空间

```shell
kube-dump image save dir ./backup --namespace production
```

该选项可以重复使用。

## 平台

不指定 `--image-platform` 时，会保存正在运行的容器所使用的平台变体。

要保存其他平台变体，请显式列出：

```shell
kube-dump image save dir ./backup \
  --image-platform linux/amd64 --image-platform linux/arm64
```

如果镜像缺少任意一个请求的平台，kube-dump 会使本次捕获失败，
并保留之前已保存的布局不变。
每个不同的可变源标签都会在捕获开始时解析一次。
该标签的所有请求平台都从固定的解析结果复制；
同一个标签不会针对每个平台重复解析。

## 仓库访问

默认情况下，kube-dump 使用运行环境中可用的凭据。

还可以使用 Pod 中的 `imagePullSecrets`：

```shell
kube-dump image save dir ./backup --image-pull-secrets
```

> [!NOTE]
> 这适用于常规的 Kubernetes Secret 配置，但不能替代所有可能的 kubelet
> 认证机制。

实际运行 kube-dump 的环境必须能够访问镜像仓库。`--image-pull-secrets` 是额外的
凭据来源。

## Registry mirror

通过映射替换源 registry 主机：

```shell
kube-dump image save dir ./backup \
  --registry-mirror registry.old.example=registry.mirror.example
```

原始引用仍会保留，但下载会使用 mirror。

## 配置文件选择

例如，以下配置文件选择 `production` namespace 中 Pod 的镜像：

```yaml
apiVersion: kube-dump/v2
kind: Profile

metadata:
  name: production
  description: Production image backup

resources: {}

images:
  selection:
    namespaces:
      - production
```

可以按 Pod 的 label、annotation 和 owner 选择 Pod，也可以按 registry、路径和标签
选择镜像引用。

以下配置片段只选择由 Deployment 所拥有的 Pod 的镜像：

```yaml
images:
  selection:
    owners:
      resources:
        - apps/v1/deployments
    containerTypes:
      - container
```

`owners` 检查 Pod 的 `ownerReferences` 链，而不是 Deployment 本身的名称。
内置 image RBAC 可读取标准 `apps` 和 `batch` workload owner。
选择自定义 owner 资源的 profile 还需要对应的 `get` 权限。

以下片段限制镜像引用：

```yaml
images:
  selection:
    references:
      - registry: ghcr.io
        repository: acme/*
        tags:
          - 'release-*'
```

完整格式请参阅[配置文件参考](../../profile.md#imagepolicy)。

## 检查

检查本地镜像副本：

```shell
kube-dump image inspect dir ./backup
```

检查 S3 中的副本：

```shell
kube-dump image inspect s3 --s3-uri s3://backup/prod/
```

命令会显示已保存的引用、digest、平台和数据大小。

## 发布到其他 registry

发布本地副本：

```shell
kube-dump image push dir ./backup registry.example.com/recovered
```

发布 S3 中的副本：

```shell
kube-dump image push s3 registry.example.com/recovered --s3-uri s3://backup/prod/
```

要只发布指定镜像，请在目标 registry 后传入引用。不提供额外引用时会发布全部镜像：

```shell
kube-dump image push dir ./backup registry.example.com/recovered \
  ghcr.io/acme/api:2.5-rc.11
```

发布开始前会校验所有选定引用，因此拼写错误不会造成部分发布。

目标 registry 必须显式指定。kube-dump 不会自动发布回源 registry。

对于复杂复制、标签映射、签名和其他操作，请使用 [ORAS]、[crane] 或 [regctl]。

[ORAS]: https://github.com/oras-project/oras
[crane]: https://github.com/google/go-containerregistry
[regctl]: https://github.com/regclient/regclient
