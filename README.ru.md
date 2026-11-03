# kube-dump · [EN] · [RU] · [ZH]

**kube-dump - утилита для сохранения состояния Kubernetes.**
Подробности — на [сайте документации][docs].

![kube-dump][logo]

В тестовом namespace, песочнице разработчика, временном стенде
или окружении для демонстрации состояние часто меняется вручную.
Кто-то обновляет Deployment, создаёт ConfigMap, пробует новый оператор,
записывает данные в PVC или запускает временный образ.
Через некоторое время это состояние может понадобиться снова -
даже если окружение уже изменилось или было удалено.
Для запуска kube-dump внутри кластера используйте
[компоненты Kustomize][kubernetes].

В таких окружениях не всегда есть GitOps,
отдельная система резервного копирования и строгий жизненный цикл.
Это не делает их неправильными,
но оставляет важные данные без переносимой копии.

kube-dump сохраняет такое состояние в понятном формате,
который можно хранить локально, в Git или S3-совместимом хранилище
и использовать при восстановлении.

## Установка

Готовые файлы последнего стабильного выпуска:

Архитектура/ОС | Linux | macOS | Windows
--------------- | ------------- | ------------- | ---------------
amd64 | [Linux amd64] | [macOS amd64] | [Windows amd64]
arm64 | [Linux arm64] | [macOS arm64] | [Windows arm64]

Все опубликованные версии доступны на странице [GitHub Releases][ghr].

Если установлен Go 1.27 или новее, можно установить текущую версию командой:

```shell
go install github.com/woozymasta/kube-dump/v2/cmd/kube-dump@v2.0.0-rc.2
```

Для запуска внутри Kubernetes доступны готовые
[Kustomize-компоненты][kubernetes].

Образ контейнера публикуется в следующих реестрах:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.2
docker.io/woozymasta/kube-dump:2.0.0-rc.2
```

Для CI, отладки и скриптов доступен вариант с shell и дополнительными
утилитами:

```text
ghcr.io/woozymasta/kube-dump:2.0.0-rc.2-debug
docker.io/woozymasta/kube-dump:2.0.0-rc.2-debug
```

Подробные варианты установки описаны в [документации][docs].

## Что сохраняется

kube-dump сохраняет три независимые части состояния Kubernetes:

* ресурсы Kubernetes - YAML-манифесты для хранения в Git, S3 или архиве;
* данные PVC - файловые архивы, которые можно читать из CSI-снимка;
* контейнерные образы - стандартный OCI Image Layout,
  включая временные образы и сборки из запросов слияния.

Можно сохранять каждый слой отдельно или использовать их вместе.

## Как использовать

Каждый тип данных сохраняется своей командой.
Например, ресурсы можно сохранить локально:

```sh
kube-dump resource save dir ./backup
```

Или отправить их в S3-совместимое хранилище:

```sh
kube-dump resource save s3 --s3-uri s3://backup/my-cluster/
```

PVC и образы сохраняются отдельными командами `pvc` и `image`.

Сохраняйте состояние перед обслуживанием или экспериментом,
до очистки временного реестра или перед удалением тестового окружения.

## Чего он не заменяет

kube-dump не заменяет Argo CD, Flux, Velero, операторы баз данных
или штатные механизмы резервного копирования приложений.

Если приложение умеет создавать корректную логическую копию базы данных,
следует продолжать использовать этот механизм.
Если инфраструктура полностью описана в Git,
Git остаётся главным источником желаемого состояния.

Задача kube-dump - зафиксировать то, что реально существует в кластере сейчас,
в понятном и переносимом виде, чтобы потом было из чего восстанавливаться.

## Профили и защита данных

Профили задают правила отбора и обработки ресурсов, PVC и образов.
Для Kubernetes-манифестов они также определяют правила очистки
и выбора полей для шифрования.

Встроенные профили:

* [backup] - для резервного копирования конфигурации,
  доступа и основных объектов операторов с удалением серверного состояния;
* [export] - для чистого и переносимого экспорта манифестов
  с удалением служебных данных и пустых полей;
* [raw] - без очистки и фильтрации, сохраняет все обнаруженные объекты.

Чувствительные значения в YAML можно шифровать
с помощью age или детерминированного AES-256-SIV.
Архивы ресурсов и PVC можно дополнительно защищать age.

## Дополнительная информация

Этот README не является полной документацией.
Смотрите [документацию в репозитории][project-docs]
или на [сайте документации][docs].
README также доступен на [английском][EN] и [китайском][ZH].

[docs]: https://kube-dump.woozymasta.ru/ru/
[project-docs]: ./docs/README.md
[ghr]: https://github.com/WoozyMasta/kube-dump/releases
[kubernetes]: ./deploy/README.md
[logo]: ./docs/assets/images/logo-wide.png
[EN]: ./README.md
[RU]: ./README.ru.md
[ZH]: ./README.zh.md
[backup]: ./internal/profile/builtin/backup.yaml
[export]: ./internal/profile/builtin/export.yaml
[raw]: ./internal/profile/builtin/raw.yaml

[Linux amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-linux-amd64
[Linux arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-linux-arm64
[macOS amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-darwin-amd64
[macOS arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-darwin-arm64
[Windows amd64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-windows-amd64.exe
[Windows arm64]: https://github.com/WoozyMasta/kube-dump/releases/download/v2.0.0-rc.2/kube-dump-windows-arm64.exe
