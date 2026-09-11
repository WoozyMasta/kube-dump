# PVC 数据

`pvc` 命令保存 `PersistentVolumeClaim` 对象的内容。
每个 PVC 都保存为单独的压缩归档，因此卷数据可以使用与 YAML 不同的计划和保留策略。

完整选项列表请参阅 [CLI 参考](../../cli.md#pvc)。

## 基本备份

```shell
kube-dump pvc save dir ./backup
```

限制为一个 namespace：

```shell
kube-dump pvc save dir ./backup --namespace production
```

按名称或 glob 选择 PVC：

```shell
kube-dump pvc save dir ./backup --pvc postgres-data --pvc 'redis-*'
```

## 备份布局和保留策略

每个 PVC 副本保存在带时间戳的目录中：

```text
volumes/
  production/
    postgres-data/
      20260904T010203Z/
        data.tar.zst
        metadata.yaml
```

默认保留每个 PVC 最新的五个副本。使用 `--keep` 修改：

```shell
kube-dump pvc save dir ./backup --keep 10
```

使用 `--keep 0` 禁用轮换并保留所有副本：

```shell
kube-dump pvc save dir ./backup --keep 0
```

## 归档格式

每个副本包含 `metadata.yaml` 和一个压缩数据流。
元数据记录格式、策略、压缩方式、未压缩大小、SHA-256 摘要和加密状态。
`sizeBytes` 是未压缩大小。

归档会保留普通文件、目录、相对符号链接、权限位、数字 UID/GID 和修改时间。
不会保留扩展属性、ACL、稀疏文件、硬链接拓扑、设备文件或其他特殊文件。
包含多个硬链接的文件会被拒绝；Windows volume agent 无法恢复非零数字 UID/GID，
也不保证完整保留 POSIX 权限位。

写入目标 PVC 前会先完整校验归档。
开始提取后恢复不是事务操作，已经写入的数据不会回滚。
从本地目录恢复时会使用编码归档的临时副本，因此临时目录需要有足够空间。
helper 会流式校验并提取，不会在 `/tmp` 中生成 PVC 大小的解码副本。

## `snapshot-copy` 策略

这是默认策略：

```shell
kube-dump pvc save dir ./backup --strategy snapshot-copy
```

流程如下：

```text
source PVC
  -> VolumeSnapshot
  -> 从快照恢复的临时 PVC
  -> 临时 Pod
  -> archive
```

该策略读取某个时间点的副本，而不是应用正在使用的实时卷。

> [!IMPORTANT]
> 需要 CSI snapshot、相应的 Kubernetes 权限以及兼容的存储驱动。

临时 helper Pod 使用当前版本的镜像。默认 pull policy 是 `IfNotPresent`；
如果同一个标签下的镜像已更新，请使用 `Always`：

```shell
kube-dump pvc save dir ./backup --image-pull-policy Always
```

使用 `--image` 和 `--helper-binary-path` 覆盖 helper 镜像或可执行文件。
helper 有一个用于小型运行时文件的临时 `/tmp`，
但恢复校验和提取不会在那里保存 PVC 大小的解码归档。

需要时可以显式选择 snapshot class：

```shell
kube-dump pvc save dir ./backup --snapshot-class csi-snapshots
```

## `pod` 策略

如果无法使用 snapshot，请选择：

```shell
kube-dump pvc save dir ./backup --strategy pod
```

临时 Pod 会直接读取源 PVC。

> [!WARNING]
> 对于正在变化的数据，这种方式可靠性较低，因为应用可能在复制期间继续写入。
> 如果存储系统支持，请对数据库和类似工作负载优先使用 `snapshot-copy`。

## 保存到 S3

```shell
kube-dump pvc save s3 --s3-uri s3://backup/prod/
```

临时 Pod 可以直接将归档发送到 S3。对于大型卷，这很重要，因为数据不会经过
kube-dump 进程。

> [!IMPORTANT]
> 因此 S3 必须能够从集群访问。

如果集群内的 S3 地址不同，请提供该地址：

```shell
kube-dump pvc save s3 --s3-uri s3://backup/prod/ \
  --s3-pod-endpoint http://seaweedfs.storage.svc:8333 \
  --s3-insecure
```

## 大小限制

超过限制时跳过单个 PVC：

```shell
kube-dump pvc save dir ./backup --max-size 500GiB
```

设置一个 namespace 中所有 PVC 的总限制：

```shell
kube-dump pvc save dir ./backup --max-namespace-size 2TiB
```

## 配置文件选择

以下配置选择 `production` 中的 PVC：

```yaml
apiVersion: kube-dump/v2
kind: Profile

metadata:
  name: production
  description: Production PVC backup

resources: {}

pvc:
  selection:
    namespaces:
      - production
    names:
      - 'postgres-*'
      - 'redis-*'
```

也可以使用 label 和 annotation。参阅[配置文件参考](../../profile.md#pvcpolicy)。

## 加密

PVC 归档使用 AGE。提供公钥以启用加密：

```shell
kube-dump pvc save dir ./backup --recipient age1...
```

加密现有归档：

```shell
kube-dump pvc encrypt data.tar.zst data.tar.zst.age --recipient age1...
```

解密归档：

```shell
kube-dump pvc decrypt data.tar.zst.age data.tar.zst --identity ./age-key.txt
```

参阅[加密](encryption.md)。

## 检查和提取

检查本地副本：

```shell
kube-dump pvc inspect dir ./backup
```

检查 S3 中的副本：

```shell
kube-dump pvc inspect s3 --s3-uri s3://backup/prod/
```

将归档提取到目录：

```shell
kube-dump pvc extract ./data.tar.zst ./restored
```

kube-dump 会恢复文件，但不会自动创建 PVC。

## 恢复到 PVC

请预先创建目标 PVC。源 PVC 和目标 PVC 分开指定，因此可以恢复到其他 namespace
或 PVC：

```shell
kube-dump pvc restore dir ./backup \
  --pvc production/postgres-data \
  --target-pvc recovery/postgres-data
```

> [!IMPORTANT]
> 目标 PVC 必须处于 `Bound` 状态、使用 `Filesystem` 模式、没有被 Pod 使用，
> 并且容量至少要达到未压缩归档的大小。

对于 `merge`，这只是最低容量检查，不会单独查询卷内已使用的空间。
在检查并写入之前，kube-dump 会为目标 PVC 获取 Kubernetes Lease。
这会阻止两个 restore 同时写入同一个 PVC，但不会停止工作负载 Pod。
PVC 使用状态仍会单独检查。

默认的 `empty-only` 策略拒绝写入非空目标 PVC。需要时显式选择 `merge` 或 `replace`：

```shell
kube-dump pvc restore dir ./backup \
  --pvc production/postgres-data \
  --target-pvc recovery/postgres-data \
  --existing merge
```

> [!CAUTION]
> `replace` 会在提取前删除目标 PVC 根目录中的现有条目。
> 失败不会回滚已经写入的数据。

默认选择最新副本。使用 `--revision 20260904T010203Z` 选择具体版本。

S3 使用对应命令：

```shell
kube-dump pvc restore s3 \
  --s3-uri s3://backup/prod/ \
  --pvc production/postgres-data \
  --target-pvc recovery/postgres-data
```

加密副本需要 `--identity`，并会以流方式解密。
命令不会停止工作负载。恢复过程会创建临时 helper Pod 和 Kubernetes Lease；
S3 restore 在需要时还会创建临时 Secret，并在操作结束后删除这些资源。

在创建 helper Pod 之前还会再次检查目标 PVC。其 UID 必须与首次检查时一致，
并且 PVC 仍须处于 `Bound` 状态、使用 `Filesystem` 模式、容量足够，
且未被运行中的 Pod 使用。

归档校验器拒绝包含超过 1,000,000 个条目的归档。
这是针对损坏归档的安全上限，不是 PVC 大小限制。
大规模备份失败时，最多保留八个 PVC 的详细错误，并单独报告被省略的详细错误数量。
