# Kubernetes 资源

`resource` 命令将 Kubernetes 对象保存为 YAML。

写入前，kube-dump 会应用配置文件。配置文件负责选择对象、删除运行时字段，
并在启用加密时保护字段值。

完整选项列表请参阅 [CLI 参考](../../cli.md#resource)。

## 保存到目录

```shell
kube-dump resource save dir ./backup
```

默认使用 `backup` 配置文件。

查看内置配置文件：

```shell
kube-dump profile show backup
```

如果需要保存对象，但不使用内置配置文件的清理和限制，可以使用 `raw`：

```shell
kube-dump resource save dir ./backup --profile raw
```

默认保存不会删除任何内容：从 Kubernetes 中消失或被新配置文件排除的对象
仍会保留在目标中。使用 `--prune` 删除上一次资源备份保存、但当前不再选择的文件：

```shell
kube-dump resource save dir ./backup --prune
```

对于目录、Git 和 S3，`--prune` 只会在采集完整成功后执行，
并且仅删除目标 ownership manifest 中记录的路径。
每次完整保存都会更新此清单；不使用 `--prune` 时会保留后续清理所需的历史。
在目录和 Git 备份中，清单路径为 `resources/.kube-dump/ownership.yaml`。
一个目标代表一个逻辑上的资源备份；独立计划任务应使用不同的目录或 S3 前缀。
目录和 Git 会先在临时位置构建资源树，S3 会先上传新 generation 再切换 current 指针。
如果采集或发布失败，现有目标不会改变。
不使用 `--strict` 时，采集会在警告后继续获取诊断；使用 `--strict` 时会在首个采集错误处停止。

> [!CAUTION]
> 请将任意用户文件放在备份目录之外。它们不属于 kube-dump 格式，
> 也不会由保存、清理或转换操作管理。

## 保存到 Git

Git 适合保存 YAML：可以使用普通的 `git diff` 审查变化，历史记录由 Git 管理：

```shell
kube-dump resource save git ./backup \
  --git-remote-url git@example.com:infra/kube-backup.git \
  --git-branch main --git-commit --git-push
```

Git 只用于资源。PVC 和容器镜像不会保存到 Git。

## 保存到 S3

```shell
kube-dump resource save s3 --s3-uri s3://backup/prod/
```

对于 S3-compatible 服务，按需要指定 endpoint 和 region。准确的选项请参阅
[CLI 参考](../../cli.md#resource-save-s3)。

## 归档

如果需要单个文件而不是 YAML 目录树，可以将资源保存为归档：

```shell
kube-dump resource save archive ./backup.tar.zst
```

也可以直接将资源归档写入 S3：

```shell
kube-dump resource save archive-s3 --s3-uri s3://backup/prod/resources.tar.zst
```

支持 Gzip 和 Zstandard。整个归档可以使用 AGE 加密。

参阅[加密](encryption.md)。

## 不创建中间文件直接输出

```shell
kube-dump resource save stdout --profile backup
```

例如，直接将结果交给 `kubectl`：

```shell
kube-dump resource save stdout --profile backup |
  kubectl diff -f -
```

> [!TIP]
> stdout 只用于命令数据，不会混入日志。

## 一次性选择

简单场景不需要单独编写配置文件。

保存一个 namespace 中的对象：

```shell
kube-dump resource save dir ./backup --namespace production
```

只保存指定的资源类型：

```shell
kube-dump resource save dir ./backup \
  --resource deployments --resource configmaps
```

按 label 选择对象：

```shell
kube-dump resource save dir ./backup \
  --label-selector 'app.kubernetes.io/part-of=my-app'
```

> [!WARNING]
> 采集警告会使保存命令失败，并且不会发布暂存结果。
> 如果希望在采集错误处立即停止，而不是继续获取对象和诊断，请添加 `--strict`。

为避免诊断信息无限增长，结果最多保留八条警告详情。最终结构化日志仍会在
`warnings` 字段中报告准确的警告总数，并在 `warnings_omitted` 字段中报告
被省略的详情数量。

所有权清单限制为 1 MiB。资源选择过大时会失败，
而不会为 `--prune` 创建不受限制的清单。

CLI 参考中包含完整的选择选项列表。

## 配置文件规则

例如，保存 ConfigMap，但排除自动创建的 `kube-root-ca.crt`：

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

以下规则从选定资源中删除运行时字段：

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

完整格式请参阅[配置文件参考](../../profile.md)。

## 加密单独字段

可以在配置文件中标记要加密的字段：

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

密钥和加密模式在运行时单独提供。

参阅[加密](encryption.md)。

## 读取已保存的资源

`resource cat` 将已保存的对象合并为 YAML 流：

```shell
kube-dump resource cat dir ./backup |
  kubectl apply -f -
```

如果保存的字段已加密，请提供 AGE 私钥：

```shell
kube-dump resource cat dir ./backup --identity ./age-key.txt |
  kubectl apply -f -
```

Git、S3 和归档也有对应的命令。

kube-dump 不会试图替代 `kubectl`、Argo CD 或 Flux
它负责保存数据并返回普通 YAML。
