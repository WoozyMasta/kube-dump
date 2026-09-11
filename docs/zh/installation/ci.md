# 从 CI 运行

在 CI 中，kube-dump 作为普通命令运行。流水线需要预先提供 Kubernetes API、
配置文件和目标存储的访问权限。

## CI 镜像

CI 任务使用带有 `-debug` 后缀的镜像：

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
```

主镜像基于 `scratch` 构建，只包含 `kube-dump` 二进制文件。
它适合直接运行该工具，但不适合作为 GitLab CI 或其他 CI runner 镜像，
因为其中没有执行任务命令所需的 Shell。

debug 镜像包含 runner 和流水线脚本常用的 Shell 及标准工具，
包括 `git`、`base64`、`curl`、`jq`、`yq`、`kubectl` 和归档工具。

> [!WARNING]
> 固定 debug 镜像的版本。不要在可复现的计划任务中使用 `latest`。

## GitLab CI：备份到同一个仓库

配置文件可以与 CI 配置放在一起，结果保存到同一个仓库。
以下是计划任务的示例：

```yaml
image: ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug

stages:
  - backup

variables:
  KUBECONFIG: "$CI_PROJECT_DIR/.kube/config"
  KUBE_DUMP_LOG_FORMAT: json
  KUBE_DUMP_NO_PROGRESS: "true"

scheduled-backup:
  stage: backup
  rules:
    - if: '$CI_PIPELINE_SOURCE == "schedule"'
  script:
    - install -d -m 700 "$(dirname "$KUBECONFIG")"
    - printf '%s' "$KUBECONFIG_B64" | base64 -d > "$KUBECONFIG"
    - chmod 600 "$KUBECONFIG"
    - kube-dump profile validate ./profiles/production.yaml
    - kube-dump resource save git ./backup \
        --profile ./profiles/production.yaml --git-push \
        --git-push-option=ci.skip
```

将 `KUBECONFIG_B64` 作为项目或组级别的 protected 变量保存。
通过 GitLab 配置 push token，不要将它写入 YAML。仅计划任务的规则会阻止备份提交
再次启动同一个任务。

提交推送后，GitLab 通常会启动新的流水线。若要跳过该流水线而不在提交信息中添加
`[skip ci]`，请传入：

```shell
--git-push-option=ci.skip
```

要向流水线传递变量，使用带有 `ci.variable` 前缀的相同选项，例如：

```shell
--git-push-option=ci.variable=KUBE_DUMP_SOURCE=scheduled
```

配置文件和目标可以分开处理：

* 在使用 `resource save git` 前，将相邻仓库克隆到工作目录；
* 通过 `KUBE_DUMP_S3_*` 变量配置 S3；
* 为 PVC 和镜像使用不同的任务和计划。除非有明确理由，大型 PVC 不应与其他备份 共用一个任务。

在项目设置或任务中直接设置 `KUBE_DUMP_S3_*` 变量：

```yaml
variables:
  KUBE_DUMP_S3_URI: s3://backup/production/
  KUBE_DUMP_S3_ENDPOINT: https://s3.example.com
  KUBE_DUMP_S3_ACCESS_KEY: replace-me
  KUBE_DUMP_S3_SECRET_KEY_FILE: $CI_PROJECT_DIR/.s3-secret
```

## GitLab CI：直接将 PVC 备份到 S3

为 PVC 使用单独的任务。S3 备份期间，集群中的临时 Pod 可以直接向存储发送归档，
因此 `KUBE_DUMP_S3_POD_ENDPOINT` 必须能够从 Kubernetes 访问。

```yaml
pvc-backup:
  stage: backup
  variables:
    KUBE_DUMP_NAMESPACE: production
    KUBE_DUMP_S3_URI: s3://backup/production/
    KUBE_DUMP_S3_ENDPOINT: https://s3.example.com
    KUBE_DUMP_S3_POD_ENDPOINT: https://s3.example.com
    KUBE_DUMP_S3_REGION: us-east-1
    KUBE_DUMP_S3_ACCESS_KEY: replace-me
    KUBE_DUMP_S3_SECRET_KEY_FILE: $CI_PROJECT_DIR/.s3-secret
  rules:
    - if: '$CI_PIPELINE_SOURCE == "schedule"'
  script:
    - printf '%s' "$S3_SECRET_KEY" > "$CI_PROJECT_DIR/.s3-secret"
    - chmod 600 "$CI_PROJECT_DIR/.s3-secret"
    - kube-dump pvc save s3 --no-progress
```

`KUBE_DUMP_S3_POD_ENDPOINT` 可能与 GitLab runner 使用的 endpoint 不同。
例如，runner 可能使用外部地址，而临时 Pod 使用集群内 S3 服务的地址。

## GitHub Actions

GitHub Actions 使用相同的模式：将 debug 镜像作为任务容器运行，
通过 Secret 或 checkout 的工作副本提供 kubeconfig、配置和存储访问权限。

```yaml
jobs:
  backup:
    if: github.event_name == 'schedule'
    runs-on: ubuntu-latest
    container: ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
    steps:
      - uses: actions/checkout@v4
      - name: Backup resources
        env:
          KUBECONFIG_B64: ${{ secrets.KUBECONFIG_B64 }}
        run: |
          printf '%s' "$KUBECONFIG_B64" | base64 -d > "$RUNNER_TEMP/kubeconfig"
          chmod 600 "$RUNNER_TEMP/kubeconfig"
          export KUBECONFIG="$RUNNER_TEMP/kubeconfig"
          kube-dump resource save s3 --s3-uri s3://backup/production/ \
            --profile ./profiles/production.yaml --no-progress
```

## 其他 CI 系统

对于 Tekton、Jenkins 和 Drone，执行相同的四个步骤：

1. 对需要 Shell 的步骤使用 `kube-dump:<version>-debug`；
1. 通过 Secret 或 runner 的标准机制提供 kubeconfig；
1. 从仓库或 Secret 挂载配置文件；
1. 关闭 progress，并选择 Git、S3 或本地工作空间。

在 Tekton 中，这通常是一个使用 debug 镜像、通过 workspace 提供配置和目标的步骤：

```yaml
steps:
  - name: backup
    image: ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
    script: |
      kube-dump resource save s3 --s3-uri s3://backup/production/ \
        --profile /workspace/source/profiles/production.yaml --no-progress
```

Kubernetes 和 S3 的具体权限模型取决于 runner。
不要将私钥或密码放入会保存到日志中的 Pipeline 清单或命令参数。
