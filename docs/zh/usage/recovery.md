# 恢复

kube-dump 不是完整的 Kubernetes 恢复系统。它以标准格式保存数据，并让你可以读取
这些数据：

* 资源 - YAML；
* 镜像 - OCI Image Layout；
* PVC - 压缩的文件系统归档。

其余恢复工作使用普通的 Kubernetes、registry 和应用工具完成。
这样恢复过程是明确的：kube-dump 不会在一个命令中隐藏 PVC 创建、镜像修改、工作负载
启动或数据迁移。

## 从这里开始

先检查保存的数据：

```shell
kube-dump resource inspect dir ./backup
kube-dump pvc inspect dir ./backup
kube-dump image inspect dir ./backup
```

检查 S3 副本时使用对应的 `inspect s3` 命令。
读取加密数据前，先准备所需的 AGE 密钥。

## 格式兼容性

保存的数据包含验证和读取所需的元数据：PVC 归档使用 `metadata.yaml`，
镜像使用标准 OCI Image Layout，资源备份使用所有权清单。
请将这些文件与保存的数据放在一起，并在移动或恢复数据时使用匹配的 kube-dump v2 版本。

## 常见恢复顺序

对于完整环境，通常可以按以下顺序操作：

1. 准备新的集群和存储；
1. 发布已保存的容器镜像；
1. 准备资源 YAML；
1. 在不启动会立即使用它们的应用的情况下创建 PVC；
1. 恢复 PVC 数据；
1. 应用或启用工作负载；
1. 验证应用。

并非每个集群都必须严格遵循此顺序，但原则是：在启动应用前先恢复依赖和数据。

## Kubernetes 资源

将已保存的资源读取为 YAML 流：

```shell
kube-dump resource cat dir ./backup > resources.yaml
```

如果字段已加密：

```shell
kube-dump resource cat dir ./backup \
  --identity ./age-identity.txt > resources.yaml
```

检查结果并应用：

```shell
kubectl diff -f resources.yaml
kubectl apply -f resources.yaml
```

正式恢复时，应决定对象的创建顺序：namespace、CRD 和 operator、RBAC、配置、PVC、
工作负载，然后是 Service、Ingress 和其他应用对象。
CRD 或 operator 不存在时，不能应用对应的 custom resource。

## 不要在数据恢复前启动应用

> [!WARNING]
> PVC 仍为空时，不要立即应用所有 Deployment 和 StatefulSet。
> 应用可能创建新的空数据库、运行迁移、改变目录结构，或者在旧数据恢复前写入新数据。

通常应先创建 PVC 和基础资源，恢复数据，然后再启动工作负载。

## 容器镜像

如果原 registry 不可用，请将已保存的镜像发布到恢复期间可访问的 registry：

```shell
kube-dump image push dir ./backup registry.example.com/recovered
kube-dump image push s3 registry.example.com/recovered --s3-uri s3://backup/prod/
```

然后让恢复后的 Pod 使用可访问的镜像名称。可以通过 DNS、mirror 或容器运行时设置
恢复旧 registry 地址，也可以使用 Kustomize、Helm values、GitOps 工具或其他 YAML
工具更新 YAML。

kube-dump 特意不引入自定义镜像重写语言。复杂的 registry 操作请使用 [ORAS]、[crane]
或 [regctl]。

## PVC

kube-dump 将选定副本恢复到已存在的 PVC，不会自动创建新的 PVC：

```shell
kube-dump pvc restore dir ./backup \
  --pvc production/postgres-data \
  --target-pvc recovery/postgres-data

kube-dump pvc restore s3 \
  --s3-uri s3://backup/prod/ \
  --pvc production/postgres-data \
  --target-pvc recovery/postgres-data
```

默认要求目标 PVC 为空。通过 `--existing` 选择 `merge` 或 `replace`；使用
`--revision` 选择具体副本。目标必须处于 Bound 状态、使用 Filesystem 模式、容量足够
容纳未压缩数据，并且不能被运行中的 Pod 挂载。

> [!CAUTION]
> `replace` 会在写入前删除目标内容。流传输失败不会回滚部分结果。

手动传输时，可以先提取归档，再将文件复制到挂载目标 PVC 的临时 Pod 中。
这种方式适合中小型卷；对于大型卷，应使用存储原生或集群内传输路径。

例如，创建一个挂载目标 PVC 的临时 Pod：

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pvc-restore
spec:
  restartPolicy: Never
  containers:
    - name: restore
      image: alpine:3.22
      command: [sh, -c, sleep infinity]
      volumeMounts:
        - name: data
          mountPath: /restore
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: postgres-data
```

应用它，并将提取的文件流式传入挂载的卷：

```shell
kubectl apply -f restore-pod.yaml
tar -C ./restored -cf - . |
  kubectl exec -i pvc-restore -- tar -C /restore -xf -
kubectl delete pod pvc-restore
```

对于非常大的卷，这种简单方法可能很慢，因为数据流会经过 Kubernetes API。

## 大型 PVC

对于数百 GB 或 TB 级数据，应优先选择不让完整数据流经过 Kubernetes API 的路径：
临时 Pod 或 Job 从 S3 下载、存储原生复制工具、CSI snapshot 和 clone、集群内的
`rsync`，或应用提供的导入机制。

kube-dump 提供可移植的源副本；传输方法应与存储系统匹配。

## 数据库和文件所有权

PVC 归档是文件系统副本，不一定是逻辑数据库备份。对于
PostgreSQL、MySQL、ClickHouse、Elasticsearch 等系统，请遵循应用自己的恢复规则
`pg_dump`、`pg_restore`、`mysqldump`、`xtrabackup`
或应用 API 可能更适合作为主要数据库备份方式。

保留权限、所有者、组、符号链接和目录结构。
临时 Pod 必须有权限恢复所需的所有者和模式。
归档还会保留文件和目录的数字 UID/GID 以及修改时间。
扩展属性、ACL、稀疏区段、设备文件和其他特殊文件不属于可移植格式。
Windows volume agent 不支持恢复非零 UID/GID。

自动 helper 以 root 运行，但只获得所需的 Linux capabilities。
其临时 `/tmp` 只保存小型运行时文件；流校验和提取不会在那里创建 PVC 大小的解码副本。
临时 Pod 和 Lease 都有生命周期上限。
运行被中断后，请根据 `app.kubernetes.io/managed-by=kube-dump` 和
`kube-dump/source-pvc` 标签检查并删除残留对象。

## 其他 StorageClass 和集群迁移

新集群不需要使用相同的 StorageClass。创建满足所需大小的新 PVC，等待其就绪，然后
将文件恢复到其中。

迁移时，将三种数据保存到共享目标：

```shell
kube-dump resource save s3 --s3-uri s3://backup/migration/
kube-dump pvc save s3 --s3-uri s3://backup/migration/
kube-dump image save s3 --s3-uri s3://backup/migration/
```

按以下方式恢复：

```text
images -> new registry
resources -> YAML
PVCs -> new volumes
```

> [!TIP]
> 在真实迁移前，先在测试环境中完整演练一次恢复过程。

## GitOps

如果 Argo CD、Flux 或其他 GitOps 工具管理资源，没有明确理由时不要直接应用保存的
YAML。Git 仍是期望状态的来源，而 kube-dump 保存观测到的状态、PVC 数据以及可能从
registry 消失的镜像。

## kube-dump 有意不处理的内容

kube-dump 不会自动：

* 为所有对象建立依赖图；
* 确定 operator 和 CRD 的安装顺序；
* 创建新的 PVC 并填充数据；
* 重写所有容器镜像引用；
* 恢复外部云服务；
* 执行数据库特定的迁移；
* 保证 `kubectl apply` 后应用已经就绪。

这些任务取决于集群和应用。kube-dump 以标准格式保留恢复所需的配置、数据和镜像。

## 定期测试恢复

检查备份的最佳方法是在隔离环境中定期恢复。最小演练需要新的测试集群、空 registry
或独立的 registry 前缀、空 PVC、已保存的镜像、PVC 数据和资源。

这可以发现外部 DNS 记录、云端 Secret、证书、StorageClass 依赖、外部数据库、无法
访问的 registry 以及未记录的人工操作。

[ORAS]: https://github.com/oras-project/oras
[crane]: https://github.com/google/go-containerregistry
[regctl]: https://github.com/regclient/regctl
