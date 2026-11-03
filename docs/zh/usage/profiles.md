# 配置文件

配置文件描述 kube-dump 应保存哪些数据。

一个文件包含相互独立的部分：

```yaml
resources:
  ...

pvc:
  ...

images:
  ...
```

完整字段列表见 JSON Schema 和[配置文件参考](../../profile.md)。

## 内置配置文件

发行版包含以下配置文件：

* `backup` - 常规备份；
* `export` - 更彻底地清理 YAML；
* `raw` - 最少处理并覆盖更多资源。

查看 `backup` 配置文件的内容：

```shell
kube-dump profile show backup
```

列出可用配置文件：

```shell
kube-dump profile ls
```

## 自定义配置文件

从最小配置文件开始：

```yaml
apiVersion: kube-dump/v2
kind: Profile

metadata:
  name: production
  description: Production backup profile

resources: {}
pvc: {}
images: {}
```

开始备份前校验配置文件：

```shell
kube-dump profile validate ./production.yaml
```

将配置文件传给保存命令：

```shell
kube-dump resource save dir ./backup --profile ./production.yaml
```

同一个文件也可以传给 `pvc save` 和 `image save`。

## 资源

以下配置文件选择 `production` namespace 中的资源类型：

```yaml
resources:
  selection:
    resources:
      - core/v1/configmaps
      - core/v1/secrets
      - apps/v1/deployments
    namespaces:
      - production
```

namespace、名称和资源表达式支持使用开头的 `!` 排除对象：

```yaml
resources:
  selection:
    resources:
      - core/v1/configmaps
      - '!core/v1/configmaps:kube-root-ca.crt'
```

> [!NOTE]
> 在 YAML 中，开头为 `!` 的值必须始终加引号。

label 和 annotation selector 支持 `In`、`NotIn`、`Exists` 和 `DoesNotExist` 操作符。

## 删除空值

启用 `omitEmpty` 可以省略明确为空的字段：

```yaml
resources:
  omitEmpty: true
```

> [!IMPORTANT]
> 空 map、空列表和 `null` 会被省略。零值、`false` 和空字符串会保留。

## 删除字段

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

## 加密字段

```yaml
resources:
  rules:
    - name: secret-data
      match:
        resources:
          - core/v1/secrets
      encrypt:
        - .data.*
```

配置文件只标记字段。密钥和加密模式通过命令行选项提供。

参阅[加密](encryption.md)。

## PVC

```yaml
pvc:
  selection:
    namespaces:
      - production
    names:
      - 'postgres-*'
      - 'redis-*'
```

PVC 也可以按 label 和 annotation 选择。

## 镜像

```yaml
images:
  selection:
    namespaces:
      - production
    references:
      - registry: ghcr.io
        repository: acme/*
        tags:
          - 'release-*'
```

也可以按 Pod 的 label、annotation、容器类型和 Pod owner 过滤。

## 管理配置文件

可以直接使用配置文件，也可以将它安装到配置文件目录：

```shell
kube-dump profile install ./production.yaml
```

安装后可以按名称引用：

```shell
kube-dump resource save dir ./backup --profile production
```

主要的配置文件管理命令：

```shell
kube-dump profile ls
kube-dump profile show production
kube-dump profile validate ./production.yaml
kube-dump profile edit production
kube-dump profile rm production
kube-dump profile which production
```

复制内置配置文件作为起点：

```shell
kube-dump profile cp builtin:backup production
```

在持续自动化中，将配置文件与其他基础设施代码一起保存在 Git 中，并在备份前运行
`profile validate`。
