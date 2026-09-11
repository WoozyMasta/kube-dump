# Запуск из CI

В CI kube-dump запускается как обычная команда.
Конвейер должен заранее предоставить доступ к Kubernetes API,
профилю и целевому хранилищу.

## Образ для CI

Для шагов CI используйте образ с суффиксом `-debug`:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
```

Основной образ намеренно собран на `scratch`
и содержит только бинарный файл `kube-dump`.
Он подходит для прямого запуска утилиты,
но **не** как образ задания GitLab CI или другого CI-раннера:
в нём нет shell, через который runner запускает команды задания.

Для CI используйте `-debug`.
В нём есть shell и стандартные утилиты,
которые нужны самому runner и скриптам конвейера:
`git`, `base64`, `curl`, `jq`, `yq`,
`kubectl`, архиваторы и другие инструменты для подготовки kubeconfig,
профилей, секретов и отправки результатов.

> [!WARNING]
> Закрепляйте версию debug-образа.
> Не используйте `latest` в расписании, которое должно быть воспроизводимым.

## GitLab CI: резервная копия в тот же репозиторий

Профиль можно хранить рядом с конфигурацией CI,
а результат - в каталоге этого же репозитория.
Пример для запуска по расписанию:

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

Переменная `KUBECONFIG_B64` должна быть защищённой в проекте или группе.
Токен для `git push` также настраивается средствами GitLab и не хранится в YAML.
Такое правило запускает задание только по расписанию
и не даёт созданному коммиту запустить его повторно.

После отправки созданного коммита GitLab обычно запускает новый pipeline.
Чтобы пропустить его и не добавлять `[skip ci]` в сообщение коммита,
передайте `--git-push-option=ci.skip`.

Если в pipeline нужно передать переменную,
используйте тот же флаг с префиксом `ci.variable`, например:

```shell
--git-push-option=ci.variable=KUBE_DUMP_SOURCE=scheduled
```

Профиль и результат можно разделить:

* для соседнего репозитория сначала клонируйте его в рабочий каталог,
  затем передайте этот каталог в `resource save git`;
* для S3 задайте параметры подключения переменными `KUBE_DUMP_S3_*`;
* для PVC и образов создайте отдельные задания
  с собственными расписаниями и лимитами.
  Большие PVC не следует сохранять в том же задании без необходимости.

Задайте параметры S3 переменными `KUBE_DUMP_S3_*`
в настройках проекта или непосредственно в задании:

```yaml
variables:
  KUBE_DUMP_S3_URI: s3://backup/production/
  KUBE_DUMP_S3_ENDPOINT: https://s3.example.com
  KUBE_DUMP_S3_ACCESS_KEY: replace-me
  KUBE_DUMP_S3_SECRET_KEY_FILE: $CI_PROJECT_DIR/.s3-secret
```

## GitLab CI: резервная копия PVC напрямую в S3

Для PVC можно использовать отдельное задание.
При сохранении в S3 временный Pod в кластере передаёт архив
непосредственно в хранилище,
поэтому `KUBE_DUMP_S3_POD_ENDPOINT` должен быть доступен из Kubernetes.

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

`KUBE_DUMP_S3_POD_ENDPOINT` может отличаться от адреса,
доступного GitLab runner. Например, runner может использовать внешний адрес,
а временный Pod - адрес S3-сервиса внутри кластера.

## GitHub Actions

Для GitHub Actions применяется тот же принцип: debug-образ используется как
контейнер задания, а kubeconfig, профиль и доступ к хранилищу приходят из
секретов или рабочей копии репозитория.

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

## Другие CI-системы

В Tekton, Jenkins и Drone нужно повторить те же четыре шага:

1. выбрать `kube-dump:<version>-debug` для шага, которому нужен shell;
1. передать kubeconfig через Secret или штатный механизм агента;
1. смонтировать профиль из репозитория или Secret;
1. выключить прогресс и выбрать Git, S3 или локальный workspace.

В Tekton это обычно один `step` с debug-образом и рабочими каталогами для
профиля и результата:

```yaml
steps:
  - name: backup
    image: ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
    script: |
      kube-dump resource save s3 --s3-uri s3://backup/production/ \
        --profile /workspace/source/profiles/production.yaml --no-progress
```

Конкретный способ выдачи прав Kubernetes и S3 зависит от раннера.
Не встраивайте закрытые ключи и пароли в манифест Pipeline или в аргументы,
которые сохраняются в журнале.
