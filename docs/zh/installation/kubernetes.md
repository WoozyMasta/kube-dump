# 在 Kubernetes 中部署

仓库提供了用于在 Kubernetes 集群中运行 `kube-dump` 的 Kustomize 组件。
将它们加入自己的组合并配置所需操作。Kustomize 已内置于 `kubectl`，无需安装 Helm。

这些清单不会替你选择操作。请从以下部分组合部署：

1. 工作负载：`CronJob` 或一次性 `Pod`；
1. `resource`、`image` 或 `pvc` 的 RBAC；
1. 包含镜像和环境设置的公共组件；
1. 仅当 `dir` 目标需要持久化时才添加本地存储。

Git 和 S3 不需要本地 PVC。对于 `dir`，可以使用容器目录；如果数据需要跨运行保留，
再添加 `storage` 组件。

## 要求

你需要：

* 支持 `kubectl apply -k` 的 `kubectl`；
* 创建所选工作负载和 RBAC 对象的权限；
* 集群节点可以访问的已发布 kube-dump 镜像；
* 使用本地存储时可用的 StorageClass；
* `pvc save` 使用 `snapshot-copy` 时所需的 CSI snapshot CRD
  snapshot controller 和兼容的存储驱动。

RBAC 组件会创建 ClusterRole 和 ClusterRoleBinding，因此应用它们通常需要集群管理员
权限。容器本身使用 `kube-dump` ServiceAccount 运行。

## 组件

### 公共组件

每个组合都添加仓库中的公共组件。它会覆盖镜像、添加标准标签、添加基础 ConfigMap，
并关闭适用于 Pod 日志的交互式 progress。它不会创建 namespace、工作负载、ServiceAccount、
权限或 PVC。请在自己的 `kustomization.yaml` 中设置 namespace。

### 工作负载

选择一个：

* `resources/cronjob/` - 使用 `concurrencyPolicy: Forbid` 的计划执行；
* `resources/pod/` - 一次性 Pod。

模板不包含 `args`。未自定义时，镜像会运行默认的 `version` 命令。
请在自己的组合中设置操作及其参数。

### 权限

选择一个 RBAC 组件：

* `components/rbac/resource/` - `resource save`；
* `components/rbac/image/` - `image save` 和 `image inspect`；
* `components/rbac/pvc/` - `pvc save` 和 `pvc restore`；
* `components/rbac/all/` - 以上三组。

PVC 组件还会授予 `coordination.k8s.io/leases` 的权限，
用于串行化写入同一个 PVC 的并发恢复操作。

`components/rbac/view/` 是用于自定义组合的附加构件。
标准 Kubernetes `view` role 不足以支持所有操作。

### 本地存储

本地存储组件只用于 `dir`：

* `components/storage/volume/` 创建 PVC `kube-dump`；
* `components/storage/cronjob/` 将它挂载到 CronJob；
* `components/storage/pod/` 将它挂载到 Pod。

工作负载存储组件已经包含 `storage/volume`，不要同时添加全部三个组件。
默认 PVC 大小为 `1Gi`；如果不够，请使用 patch 覆盖。

## 示例：将资源保存到本地 PVC

下面的配置创建一个 nightly CronJob，将内置 `backup` 配置文件中的资源保存到
`/data/backup`。

在自己的仓库中创建以下结构：

```text
kdump/
├── kustomization.yaml
└── patches/
    └── resource-save-cronjob.yaml
```

`kdump/kustomization.yaml`：

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: kube-dump

resources:
  - https://github.com/WoozyMasta/kube-dump//deploy/resources/cronjob?ref=v2.0.0-rc.1

components:
  - https://github.com/WoozyMasta/kube-dump//deploy?ref=v2.0.0-rc.1
  - https://github.com/WoozyMasta/kube-dump//deploy/components/rbac/resource?ref=v2.0.0-rc.1
  - https://github.com/WoozyMasta/kube-dump//deploy/components/storage/cronjob?ref=v2.0.0-rc.1

configMapGenerator:
  - name: kube-dump
    behavior: merge
    literals:
      - KUBE_DUMP_NAMESPACE=production
      - KUBE_DUMP_PROFILE=backup

patches:
  - path: patches/resource-save-cronjob.yaml
```

`kdump/patches/resource-save-cronjob.yaml`：

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: kube-dump
spec:
  schedule: "0 1 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: kube-dump
              args:
                - resource
                - save
                - dir
                - /data/backup
```

## 配置和 Secret

公共组件通过 `envFrom` 将 ConfigMap 和 Secret 连接到工作负载。
将操作和目标放入 `args`，将变化的选项配置为环境变量。
对于 `--some-option`，使用 `KUBE_DUMP_SOME_OPTION`。

示例中的 `configMapGenerator` 扩展了公共 ConfigMap。`merge` 保留基础值，并覆盖同名
键。`replace` 替换整个 ConfigMap，因此必须列出所有必需值。`create` 创建另一个
ConfigMap，不适合扩展 `kube-dump`。

不要将 Secret 值提交到 Kustomize 文件中。请单独创建 Secret：

```shell
kubectl create secret generic kube-dump \
  --namespace kube-dump \
  --from-literal=KUBE_DUMP_IDENTITY_PASSPHRASE='replace-me'
```

请使用 Secret Manager 或 CI Secret 提供真实值，不要在会保存到 Shell 历史的命令中写入
literal。文件型密钥和配置文件应单独挂载；环境变量存放挂载后的文件路径。

`?ref=v2.0.0-rc.1` 用于固定 Kustomize 清单版本。
升级 kube-dump 时改为所需的 release tag。

## 验证和应用

先渲染清单：

```shell
kubectl kustomize ./kdump > kube-dump.yaml
```

创建 namespace 并应用组合：

```shell
kubectl create namespace kube-dump
kubectl apply -k ./kdump
```

检查 CronJob 和 PVC：

```shell
kubectl get cronjob,pvc -n kube-dump
kubectl describe cronjob kube-dump -n kube-dump
```

使用 Job 立即执行相同场景：

```shell
kubectl create job --from=cronjob/kube-dump kube-dump-manual -n kube-dump
kubectl logs -f -n kube-dump job/kube-dump-manual
```

## 一次性 Pod

一次性操作使用 `resources/pod/` 并 patch 一个 `Pod`：

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: kube-dump

resources:
  - https://github.com/WoozyMasta/kube-dump//deploy/resources/pod?ref=v2.0.0-rc.1

components:
  - https://github.com/WoozyMasta/kube-dump//deploy?ref=v2.0.0-rc.1
  - https://github.com/WoozyMasta/kube-dump//deploy/components/rbac/resource?ref=v2.0.0-rc.1
  - https://github.com/WoozyMasta/kube-dump//deploy/components/storage/pod?ref=v2.0.0-rc.1

patches:
  - path: patches/resource-save-pod.yaml
```

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: kube-dump
spec:
  containers:
    - name: kube-dump
      args:
        - resource
        - save
        - dir
        - /data/backup
```

应用并检查 Pod：

```shell
kubectl apply -k ./kdump
kubectl logs -f -n kube-dump pod/kube-dump
kubectl get pod -n kube-dump
```

## 其他操作和目标

工作负载和 RBAC 独立于目标。修改工作负载 patch 和环境配置。
`args` 中只保留操作及位置参数形式的目标路径；配置文件、namespace、PVC 策略、重试
次数和其他选项通过 ConfigMap 或 Secret 提供。

镜像示例：

```yaml
args:
  - image
  - save
  - dir
  - /data/backup
```

配置文件必须包含镜像选择规则。镜像会保存到所选目标中的 `images` 目录。

PVC 示例：

```yaml
args:
  - pvc
  - save
  - dir
  - /data/backup
```

在 `configMapGenerator` 中添加 `KUBE_DUMP_NAMESPACE` 和
`KUBE_DUMP_STRATEGY=snapshot-copy`。`snapshot-copy` 会创建临时 snapshot、
克隆 PVC 和 helper Pod。集群必须支持 CSI snapshot
ServiceAccount 必须拥有 `rbac/pvc` 权限。
不支持 snapshot 的集群可以在直接读取安全时使用 `pod`。

对于 Git 和 S3，不要添加本地存储。S3 参数示例：

```yaml
args:
  - resource
  - save
  - s3
```

通过 `KUBE_DUMP_S3_URI` 设置 S3 URI，例如在 `configMapGenerator` 中：

```yaml
literals:
  - KUBE_DUMP_S3_URI=s3://backup/production/resources
```

通过环境变量或 Secret 传递 Git 和 S3 凭据。参阅 [CLI 参考](../../cli.md)。
PVC S3 备份使用单独的 `--s3-pod-endpoint`，它必须能从 helper Pod 访问。

## Namespace 和本地清单

修改 `kustomization.yaml` 中的 `namespace` 可以使用其他 namespace。
Kustomize 会更新 ServiceAccount 和 ClusterRoleBinding 引用，不需要手动替换。

要在本地检查或修改清单，请克隆仓库并将远程引用替换为：

```text
../../deploy/resources/cronjob
../../deploy
../../deploy/components/rbac/resource
../../deploy/components/storage/cronjob
```

## 在同一个 namespace 中使用多个组合

所有基础组件都使用名称 `kube-dump`
因此独立的 resource 和 PVC 组合如果不自定义会 发生冲突。添加不同的前缀：

```yaml
namePrefix: resources-
```

另一个组合使用：

```yaml
namePrefix: pvc-
```

Kustomize 会更新 ServiceAccount、ConfigMap、Secret、PVC 和 RBAC 对象的引用。
外部创建的 Secret 也必须使用带前缀的名称。

公共 ConfigMap 设置了 `disableNameSuffixHash: true`
因此在有意共享同一设置时名称保持稳定。

除非你明确构建自定义组合，否则不要添加多个工作负载或 RBAC 组件；它们包含同名对象。

## 故障排除

从渲染后的清单和工作负载状态开始：

```shell
kubectl kustomize ./kdump
kubectl get pods,jobs -n kube-dump
kubectl describe pod -n kube-dump -l app.kubernetes.io/name=kube-dump
kubectl logs -n kube-dump -l app.kubernetes.io/name=kube-dump --all-containers
```

无需运行备份即可检查 ServiceAccount 权限：

```shell
kubectl auth can-i --list --as=system:serviceaccount:kube-dump:kube-dump
```

使用 `pvc save` 时，还要检查 `VolumeSnapshotClass`、临时 snapshot 状态，
以及集群中 helper 镜像是否可用。
