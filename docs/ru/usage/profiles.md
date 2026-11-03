# Профили

Профиль описывает, какие данные kube-dump должен сохранять.

В одном файле есть независимые разделы:

```yaml
resources:
  ...

pvc:
  ...

images:
  ...
```

Полный перечень полей приведён в [справочнике профиля](../../profile.md).

## Встроенные профили

В поставку входят следующие профили:

* `backup` - обычная резервная копия;
* `export` - более очищенный YAML;
* `raw` - минимальная обработка и широкий охват ресурсов.

Посмотрите содержимое профиля `backup`:

```shell
kube-dump profile show backup
```

Выведите список доступных профилей:

```shell
kube-dump profile ls
```

## Свой профиль

Начните с минимальной заготовки профиля:

```yaml
apiVersion: kube-dump/v2
kind: Profile

metadata:
  name: production
  description: Профиль резервного копирования production

resources: {}
pvc: {}
images: {}
```

Проверьте профиль до запуска резервного копирования:

```shell
kube-dump profile validate ./production.yaml
```

Передайте профиль команде сохранения:

```shell
kube-dump resource save dir ./backup --profile ./production.yaml
```

Тот же файл можно передать `pvc save` и `image save`.

## Ресурсы

Следующий профиль сохраняет выбранные виды ресурсов
только из пространства имён `production`:

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

Исключения для пространства имён,
имена и выражения выбора ресурса задаются префиксом `!`:

```yaml
resources:
  selection:
    resources:
      - core/v1/configmaps
      - '!core/v1/configmaps:kube-root-ca.crt'
```

> [!NOTE]
> Значения с `!` лучше всегда брать в кавычки.

Для селекторов меток и аннотаций доступны операторы
`In`, `NotIn`, `Exists` и `DoesNotExist`.

## Удаление пустых значений

Чтобы не записывать явно пустые поля, включите `omitEmpty`:

```yaml
resources:
  omitEmpty: true
```

> [!IMPORTANT]
> Пустые карты, списки и `null` будут пропущены.
> Нулевые значения, `false` и пустые строки сохраняются.

## Удаление полей

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

## Шифруемые поля

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

Профиль только отмечает поля.
Ключ и способ шифрования передаются флагами команды.

Подробнее: [Шифрование](encryption.md).

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

Можно отбирать PVC по меткам и аннотациям.

## Образы

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

Кроме ссылок на образы можно учитывать метки Pod, аннотации,
тип контейнера и владельца Pod.

## Управление профилями

Профиль можно использовать прямо как файл или установить в каталог профилей.

```shell
kube-dump profile install ./production.yaml
```

После установки в команде можно указывать только имя профиля:

```shell
kube-dump resource save dir ./backup --profile production
```

Основные команды управления профилями:

```shell
kube-dump profile ls
kube-dump profile show production
kube-dump profile validate ./production.yaml
kube-dump profile edit production
kube-dump profile rm production
kube-dump profile which production
```

Встроенный профиль можно скопировать как основу:

```shell
kube-dump profile cp builtin:backup production
```

Для постоянной автоматизации удобно хранить профиль в Git
рядом с остальной инфраструктурой
и проверять его через `profile validate` перед запуском.
