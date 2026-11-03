# Использование CLI

На этой странице описаны общие правила командной строки `kube-dump`.
Полный список команд и флагов находится в
[справочнике CLI](../../cli.md).

## Справка

Справку можно запросить флагом `--help` или коротким флагом `-h`:

```shell
kube-dump --help
kube-dump resource -h
kube-dump resource save -h
```

Есть и отдельная команда `help`,
которая принимает путь команды отдельными аргументами:

```shell
kube-dump help
kube-dump help resource
kube-dump help resource save
```

Полный список команд и флагов находится в [справочнике CLI](../../cli.md).

## Переменные окружения

Для переменных окружения CLI использует префикс `KUBE_DUMP_`.
Имя переменной строится из длинного имени флага:
оно переводится в верхний регистр, а каждый `-` заменяется на `_`:

```text
--profile         -> KUBE_DUMP_PROFILE
--retry-attempts  -> KUBE_DUMP_RETRY_ATTEMPTS
--no-progress     -> KUBE_DUMP_NO_PROGRESS
```

В справочнике CLI переменная окружения указана рядом с соответствующим флагом.

Явно переданный флаг имеет приоритет над переменной окружения.
По умолчанию загружается файл `.env`.
Флаг `--env-file PATH` выбирает другой файл,
`--no-env` отключает его загрузку, а `--env-override`
разрешает значениям из `.env` заменить уже заданные переменные процесса.

## Повторяемые флаги в переменных окружения

Повторяемый флаг можно указывать несколько раз:

```shell
kube-dump resource save stdout \
  --namespace production \
  --namespace staging \
  --resource apps/v1/deployments \
  --resource core/v1/configmaps
```

Для повторяемых флагов `kube-dump` в переменной окружения используется запятая:

```shell
export KUBE_DUMP_NAMESPACE='production,staging'
```

В PowerShell:

```powershell
$env:KUBE_DUMP_NAMESPACE = 'production,staging'
```

Та же запятая используется для повторяемых параметров, например
`--git-push-option`.

Некоторые флаги принимают список пар в формате `ключ=значение`.
У `--registry-mirror` знак `=` разделяет ключ и значение,
а отдельные записи разделяются запятой:

```shell
export KUBE_DUMP_REGISTRY_MIRROR='registry.example.com=mirror.example.com,ghcr.io=mirror.example.net'
```

Если само значение содержит запятую,
передавайте такие значения отдельными флагами, а не одной переменной окружения.

## Авто-дополнение команд

Встроенная команда `completion` создаёт дополнение для Bash, Zsh и PowerShell.
Если путь вывода не указан, скрипт печатается в stdout.

Bash:

```shell
. <(kube-dump completion --shell bash)
```

Чтобы сохранить авто-дополнение команд постоянно:

```shell
mkdir -p ~/.local/share/bash-completion/completions
kube-dump completion --shell bash \
  ~/.local/share/bash-completion/completions/kube-dump
```

Zsh:

```shell
kube-dump completion --shell zsh > ~/.zfunc/_kube_dump
```

PowerShell:

```powershell
$path = Join-Path $HOME 'Documents/PowerShell/Completions/kube-dump.ps1'
New-Item -ItemType Directory -Force (Split-Path $path) | Out-Null
kube-dump completion --shell pwsh $path
. $path
```

## Другие форматы документации

CLI умеет выводить описание команд в нескольких форматах:

```shell
kube-dump docs md docs/cli.md --program-name kube-dump --style posix --toc
kube-dump docs html docs/cli.html --program-name kube-dump --style posix
kube-dump docs man kube-dump.1 --program-name kube-dump --style posix
kube-dump docs json docs/cli.json
```

Если путь вывода не указан, результат печатается в stdout.

## Служебная скрытая команда `volume`

`volume` используется внутри временных Pod для чтения,
записи и передачи данных PVC.
Для справки:

```shell
kube-dump help volume
```

## JSON Schema профиля

Схема профиля встроена в бинарный файл и может быть выведена без сети:

```shell
kube-dump profile schema > profile.schema.json
```

Для проверки YAML по схеме, в VS Code установите расширение YAML
[`redhat.vscode-yaml`](https://marketplace.visualstudio.com/items?itemName=redhat.vscode-yaml)
и добавьте в начало профиля:

```yaml
# yaml-language-server: $schema=./profile.schema.json
apiVersion: kube-dump/v2
kind: Profile
```

Можно ссылаться и на схему в репозитории:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/WoozyMasta/kube-dump/HEAD/pkg/profile/schema/profile.schema.json
```

Для воспроизводимой проверки закрепляйте URL на конкретном теге релиза,
а не на `HEAD`.

Проверяйте профиль в CI до запуска резервного копирования:

```shell
kube-dump profile validate ./profile.yaml
```
