# Установка и запуск

kube-dump распространяется как один исполняемый файл и как образ контейнера.
Его можно запускать вручную, из CI или внутри Kubernetes.

## Установка исполняемого файла

Готовые файлы последнего стабильного выпуска:

Архитектура/ОС | Linux         | macOS         | Windows
 ------------- | ------------- | ------------- | ---------------
amd64          | [Linux amd64] | [macOS amd64] | [Windows amd64]
arm64          | [Linux arm64] | [macOS arm64] | [Windows arm64]

Все опубликованные версии доступны на странице [GitHub Releases].

Если установлен Go 1.27 или новее, можно установить текущую версию командой:

```shell
go install github.com/woozymasta/kube-dump/v2/cmd/kube-dump@latest
```

Проверьте установку:

```shell
kube-dump version
```

## Локальный запуск

По умолчанию kube-dump использует kubeconfig и текущий контекст Kubernetes:

```shell
kube-dump resource save dir ./backup
```

Другой kubeconfig или контекст можно указать явно:

```shell
kube-dump resource save dir ./backup \
  --kubeconfig ~/.kube/config --context production
```

Практические варианты установки в Kubernetes описаны в разделе
[Развёртывание в Kubernetes](kubernetes.md),
а запуск из CI - в [Интеграции с CI](ci.md).

## Контейнерные образы

Образы публикуются в следующих реестрах:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1
docker.io/woozymasta/kube-dump:2.0.0-rc.1
```

Отдельно публикуется debug-вариант с оболочкой
и утилитами для отладки и скриптов CI:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.1-debug
docker.io/woozymasta/kube-dump:2.0.0-rc.1-debug
```

Для автоматизации закрепляйте конкретную версию образа
вместо плавающего тега `latest`.

Основной образ минимален и предназначен для запуска kube-dump.
Суффикс `-debug` обозначает образ с оболочкой
и утилитами для CI-скриптов и диагностики.

## Права доступа

Права Kubernetes зависят от команды и выбранного профиля.
В пользовательской Kustomize-композиции
выберите минимальный компонент `components/rbac/resource`,
`components/rbac/image` или `components/rbac/pvc`.
Подробности приведены в разделе об установке в Kubernetes.

<!-- links -->

[Linux amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-linux-amd64
[Linux arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-linux-arm64
[macOS amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-darwin-amd64
[macOS arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-darwin-arm64
[Windows amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-windows-amd64.exe
[Windows arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.1/kube-dump-windows-arm64.exe
[GitHub Releases]: https://github.com/WoozyMasta/kube-dump/releases
