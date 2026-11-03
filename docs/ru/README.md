# kube-dump

![Логотип kube-dump](../assets/images/logo-wide.png)

kube-dump сохраняет три части состояния Kubernetes:

* ресурсы Kubernetes в YAML;
* данные PVC;
* контейнерные образы, которые используют Pod.

Эти части независимы.
Можно сохранять только YAML, только тома или только образы.
Для полной копии окружения обычно нужны все три.

Полный список команд и флагов находится в [справочнике команд](../cli.md),
а все поля профиля - в [справочнике профиля](../profile.md).
Практические правила переменных окружения, автодополнения и генерации
документации описаны в разделе [использование CLI](usage/cli-usage.md).

Перед первым запуском установите kube-dump по инструкции
[установка и запуск](installation/overview.md).

## Быстрый старт

Сохраните ресурсы в локальный каталог:

```shell
kube-dump resource save dir ./backup
```

Сохраните данные PVC в локальный каталог:

```shell
kube-dump pvc save dir ./backup
```

Сохраните используемые контейнерные образы в локальный каталог:

```shell
kube-dump image save dir ./backup
```

Для сохранения в S3 используются те же команды;
меняется только тип приёмника и путь к нему:

```shell
kube-dump resource save s3 --s3-uri s3://backup/prod/
kube-dump pvc save s3 --s3-uri s3://backup/prod/
kube-dump image save s3 --s3-uri s3://backup/prod/
```

Один локальный каталог или один префикс S3
можно использовать для всех трёх видов данных.

## Ресурсы

`resource` читает объекты Kubernetes, применяет профиль и сохраняет YAML.

```shell
kube-dump resource save dir ./backup --profile backup
```

Сохранённые YAML можно сразу передать в `kubectl`:

```shell
kube-dump resource cat dir ./backup | kubectl apply -f -
```

Подробнее: [Ресурсы Kubernetes](usage/resource.md).

## PVC

`pvc` сохраняет файловую систему каждого выбранного PVC в отдельный архив.

```shell
kube-dump pvc save dir ./backup
```

По умолчанию kube-dump использует снимок тома и читает его копию,
не обращаясь напрямую к работающему PVC приложения.

Подробнее: [Данные PVC](usage/pvc.md).

## Образы

`image` смотрит на фактически запущенные Pod
и сохраняет используемые ими контейнерные образы.

```shell
kube-dump image save dir ./backup
```

Образы хранятся как обычный OCI Image Layout,
без собственного формата kube-dump.

Подробнее: [Контейнерные образы](usage/image.md).

## Профили

Профиль задаёт правила сразу для ресурсов, PVC и образов.

```shell
kube-dump profile show backup
```

Можно использовать встроенный профиль,
файл из Git или установленный локальный профиль.

Подробнее: [Профили](usage/profiles.md).

## Шифрование

Отдельные поля YAML можно шифровать AGE или AES-SIV.

Архивы ресурсов и PVC шифруются AGE.

Подробнее: [Шифрование](usage/encryption.md).

## Как запускать

kube-dump можно запускать вручную, из CI или по расписанию внутри Kubernetes.

Подробнее: [Установка и запуск](installation/overview.md).
