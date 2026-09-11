# Running from CI

In CI, kube-dump runs as a regular command.
The pipeline must provide access to the Kubernetes API,
the profile, and the destination storage beforehand.

## CI image

Use the image with the `-debug` suffix for CI jobs:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
```

The main image is built on `scratch` and contains only the `kube-dump` binary.
It is suitable for running the utility directly,
but not as a GitLab CI or other CI runner image
because it has no shell for executing job commands.

The debug image contains a shell and standard utilities used by the runner
and pipeline scripts, including `git`, `base64`, `curl`, `jq`, `yq`, `kubectl`,
and archive tools.

> [!WARNING]
> Pin the debug image version. Do not use `latest` for a reproducible schedule.

## GitLab CI: backup to the same repository

The profile can live next to the CI configuration,
while the result is stored in the same repository.
Example for a scheduled run:

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

Keep `KUBECONFIG_B64` protected at project or group level.
Configure the Git push token through GitLab rather than storing it in YAML.
The schedule-only rule prevents the backup commit
from starting the same job again.

After the commit is pushed, GitLab normally starts a new pipeline.
To skip it without adding `[skip ci]` to the commit message, pass:

```shell
--git-push-option=ci.skip
```

To pass a variable to the pipeline,
use the same option with the `ci.variable` prefix, for example:

```shell
--git-push-option=ci.variable=KUBE_DUMP_SOURCE=scheduled
```

The profile and destination can be separated:

* clone a neighboring repository into the work directory
  before using `resource save git`;
* configure S3 through `KUBE_DUMP_S3_*` variables;
* use separate jobs and schedules for PVCs and images.
  Large PVCs should not share a job with other backups without a clear reason.

Set S3 settings through `KUBE_DUMP_S3_*` variables in the project settings
or directly in the job:

```yaml
variables:
  KUBE_DUMP_S3_URI: s3://backup/production/
  KUBE_DUMP_S3_ENDPOINT: https://s3.example.com
  KUBE_DUMP_S3_ACCESS_KEY: replace-me
  KUBE_DUMP_S3_SECRET_KEY_FILE: $CI_PROJECT_DIR/.s3-secret
```

## GitLab CI: direct PVC backup to S3

Use a separate job for PVCs.
During an S3 backup, a temporary Pod in the cluster
can send the archive directly to storage,
so `KUBE_DUMP_S3_POD_ENDPOINT` must be reachable from Kubernetes.

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

`KUBE_DUMP_S3_POD_ENDPOINT` may differ from the endpoint
available to the GitLab runner.
For example, the runner may use an external address
while the temporary Pod uses the in-cluster S3 service address.

## GitHub Actions

GitHub Actions follows the same model:
use the debug image as the job container, and provide kubeconfig, profile,
and storage access through secrets or the checked-out working copy.

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

## Other CI systems

In Tekton, Jenkins, and Drone, follow the same four steps:

1. use `kube-dump:<version>-debug` for a step that needs a shell;
1. provide kubeconfig through a Secret or the runner's standard mechanism;
1. mount the profile from the repository or a Secret;
1. disable progress and choose Git, S3, or the local workspace.

In Tekton this is usually one step with the debug image
and workspaces for the profile and destination:

```yaml
steps:
  - name: backup
    image: ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
    script: |
      kube-dump resource save s3 --s3-uri s3://backup/production/ \
        --profile /workspace/source/profiles/production.yaml --no-progress
```

The exact Kubernetes and S3 permission model depends on the runner.
Do not put private keys or passwords in Pipeline manifests
or command arguments that are kept in logs.
