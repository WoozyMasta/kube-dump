# Запуск в Kubernetes

Репозиторий поставляет Kustomize-компоненты
для запуска `kube-dump` в кластере Kubernetes.
Их можно подключить к собственной композиции и настроить под нужную операцию.
Kustomize встроен в `kubectl`,
поэтому отдельная установка Helm или другого инструмента не нужна.

Манифесты не выбирают команду за пользователя.
Вы собираете собственную композицию из четырёх частей:

1. рабочая нагрузка - `CronJob` или одноразовый `Pod`;
1. набор RBAC для операции `resource`, `image` или `pvc`;
1. общий компонент с образом, метками и настройками окружения;
1. локальное PVC для файлового назначения -
  только если оно действительно нужно.

Для Git и S3 отдельный PVC не требуется:
контейнер передаёт данные напрямую в выбранное хранилище.
Для назначения `dir` нужен каталог внутри контейнера,
а для постоянного хранения между запусками лучше подключить компонент `storage`.

## Предварительные требования

Нужны:

* `kubectl` с поддержкой `kubectl apply -k`;
* права создавать выбранную рабочую нагрузку и связанные RBAC-объекты;
* опубликованный образ `kube-dump` и доступ узлов к его реестру;
* StorageClass, если используется компонент локального хранилища;
* для `pvc save` со стратегией `snapshot-copy` -
  установленные CRD для CSI-снимков,
  контроллер снимков и совместимый драйвер хранения.

RBAC-компоненты создают кластерные роли и `ClusterRoleBinding`.
Поэтому их применение обычно требует прав администратора кластера.
Сам контейнер после установки работает через ServiceAccount `kube-dump`.

## Компоненты

### Общий компонент

Общий компонент репозитория нужно подключать к каждой композиции.
Он:

* переопределяет образ и добавляет стандартные метки;
* добавляет ConfigMap с базовыми настройками `kube-dump`;
* включает отключение интерактивного прогресса, подходящее для логов Pod.

Он не создаёт namespace, рабочую нагрузку, ServiceAccount, права или PVC.
Namespace задаётся в пользовательском `kustomization.yaml`.

### Рабочая нагрузка

Выберите одну из заготовок:

* `resources/cronjob/` - плановый запуск с `concurrencyPolicy: Forbid`;
* `resources/pod/` - одноразовый Pod.

Заготовки не содержат `args`.
Без кастомизации контейнер выполнит команду образа по умолчанию (`version`).
Операцию и её параметры нужно задать в своей композиции.

### Права

Выберите один компонент RBAC:

* `components/rbac/resource/` - `resource save`;
* `components/rbac/image/` - `image save` и `image inspect`;
* `components/rbac/pvc/` - `pvc save` и `pvc restore`;
* `components/rbac/all/` - объединяет права всех трёх групп.

Компонент PVC также даёт доступ к `coordination.k8s.io/leases`,
который нужен для сериализации одновременных восстановлений в один PVC.

Каждый из этих компонентов добавляет общий ServiceAccount
и соответствующий `ClusterRoleBinding`.
Компонент `components/rbac/view/` размещен
как дополнительный строительный блок для собственных композиций.
Стандартной роли Kubernetes `view` недостаточно для всех операций `kube-dump`.

### Локальное хранилище

Компоненты `storage` нужны только для назначения `dir`:

* `components/storage/volume/` - создаёт PVC `kube-dump`;
* `components/storage/cronjob/` - добавляет этот PVC и монтирование в CronJob;
* `components/storage/pod/` - добавляет этот PVC и монтирование в Pod.

Компоненты `storage/cronjob` и `storage/pod` уже подключают `storage/volume`,
поэтому одновременно добавлять все три компонента не нужно.
Размер PVC по умолчанию - `1Gi`.
Если этого недостаточно, измените размер PVC собственным патчем.

## Базовая композиция: ресурсы в локальное PVC

Ниже создаётся CronJob, который каждую ночь
сохраняет ресурсы из встроенного профиля `backup` в `/data/backup`.

Создайте в своём репозитории такую структуру:

```text
kdump/
├── kustomization.yaml
└── patches/
    └── resource-save-cronjob.yaml
```

`kdump/kustomization.yaml`:

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

`kdump/patches/resource-save-cronjob.yaml`:

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

## Конфигурация и секреты

Общий компонент подключает к рабочей нагрузке ConfigMap и Secret через envFrom.
Поэтому операцию и назначение достаточно указать в `args`,
а изменяемые параметры задавайте переменными окружения.

В примере выше `configMapGenerator` расширяет ConfigMap общего компонента.
Для флага `--some-option` используется переменная `KUBE_DUMP_SOME_OPTION`.
Базовые значения компонента сохраняются при `behavior: merge`,
а одноимённые ключи переопределяются.

`behavior: replace` полностью заменяет данные ConfigMap,
поэтому при нём нужно перечислить все требуемые значения.
`behavior: create` создаёт новый ConfigMap;
для расширения `kube-dump` он не подходит и приведёт к конфликту имени.

Если профиль смонтирован в контейнер или добавлен в образ,
задайте его путь ключом `KUBE_DUMP_PROFILE` в этом генераторе.

Секретные значения не добавляйте в Kustomize-файлы.
Создайте Secret отдельно от репозитория, например:

```shell
kubectl create secret generic kube-dump \
  --namespace kube-dump \
  --from-literal=KUBE_DUMP_IDENTITY_PASSPHRASE='replace-me'
```

Для реального значения используйте менеджер секретов или секрет CI,
а не передавайте его через команду, которая попадёт в историю shell.
Файловые ключи и профили нужно отдельно смонтировать в контейнер;
переменная окружения в таком случае содержит путь к смонтированному файлу.

Ссылки `?ref=v2.0.0-rc.1` закрепляют версию исходных Kustomize-манифестов.
При обновлении kube-dump меняйте их на нужный тег релиза
и проверяйте итоговую композицию.

## Проверка и применение

Сначала отрендерите манифесты и убедитесь, что образ, пространство имён,
аргументы, монтирование и права выглядят ожидаемо:

```shell
kubectl kustomize ./kdump > kube-dump.yaml
```

Затем создайте пространство имён и примените композицию:

```shell
kubectl create namespace kube-dump
kubectl apply -k ./kdump
```

Проверьте CronJob и PVC:

```shell
kubectl get cronjob,pvc -n kube-dump
kubectl describe cronjob kube-dump -n kube-dump
```

Запустить тот же сценарий сразу, не дожидаясь расписания,
можно отдельным Job из CronJob:

```shell
kubectl create job --from=cronjob/kube-dump kube-dump-manual -n kube-dump
kubectl logs -f -n kube-dump job/kube-dump-manual
```

## Запуск одного Pod

Для разовой операции замените CronJob на Pod и направьте патч на `Pod`:

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

Примените Pod и прочитайте его логи:

```shell
kubectl apply -k ./kdump
kubectl logs -f -n kube-dump pod/kube-dump
kubectl get pod -n kube-dump
```

## Другие операции и назначения

Рабочая нагрузка и RBAC не зависят от конкретного назначения.
Меняется только патч рабочей нагрузки и конфигурация окружения.

В `args` оставляйте только операцию и позиционные пути назначения.
Профиль, namespace, стратегию PVC, retry и прочие параметры
задавайте через ConfigMap или Secret.

### Образы

Для сохранения образов используйте `rbac/image` и,
если нужен локальный каталог, `storage/cronjob` или `storage/pod`:

```yaml
args:
  - image
  - save
  - dir
  - /data/backup
```

Профиль должен содержать правила выбора образов.
Образы сохраняются в каталоге `images` внутри указанного назначения.

### PVC

Для сохранения данных PVC используйте `rbac/pvc`:

```yaml
args:
  - pvc
  - save
  - dir
  - /data/backup
```

Для этого примера добавьте `KUBE_DUMP_NAMESPACE` и
`KUBE_DUMP_STRATEGY=snapshot-copy` в `configMapGenerator`
из раздела "Конфигурация и секреты".

Стратегия `snapshot-copy` создаёт временные снимки,
клонированный PVC и вспомогательный Pod.
Кластер должен поддерживать CSI-снимки,
а ServiceAccount должен иметь права из компонента `rbac/pvc`.
Для кластеров без снимков используйте стратегию `pod`,
если выбранные PVC можно безопасно читать через вспомогательный Pod.

### Git и S3

Для Git и S3 компонент локального хранилища не подключается.
Пример аргументов для S3:

```yaml
args:
  - resource
  - save
  - s3
```

URI S3 задайте переменной `KUBE_DUMP_S3_URI`, например в `configMapGenerator`:

```yaml
literals:
  - KUBE_DUMP_S3_URI=s3://backup/production/resources
```

Параметры доступа к S3 и Git передайте через переменные окружения или Secret.
Полный список аргументов и соответствующих переменных смотрите в
[справочнике CLI](../../cli.md).

Для PVC в S3 используется отдельный endpoint,
доступный из вспомогательного Pod: `--s3-pod-endpoint`.
Этот endpoint не обязан совпадать с endpoint, доступным основному контейнеру.

## Пространство имён и локальная копия манифестов

Для другого namespace
достаточно изменить `namespace` в пользовательском `kustomization.yaml`.
Сервисный аккаунт и `subjects` всех `ClusterRoleBinding`
будут согласованы Kustomize-компонентом автоматически;
ручные `replacements` для этого не нужны.

Если манифесты нужно проверять или изменять локально,
скачайте исходники репозитория и замените удалённые ссылки локальными путями:

```text
../../deploy/resources/cronjob
../../deploy
../../deploy/components/rbac/resource
../../deploy/components/storage/cronjob
```

## Несколько запусков в одном пространстве имён

Все базовые компоненты используют имя `kube-dump`.
Поэтому две независимые композиции, например для `resource` и `pvc`,
без дополнительных настроек будут конфликтовать по именам.

Добавьте разные префиксы в пользовательские `kustomization.yaml`:

```yaml
namePrefix: resources-
```

и, например, для второй композиции:

```yaml
namePrefix: pvc-
```

Kustomize автоматически обновит ссылки на
ServiceAccount, ConfigMap, Secret, PVC и RBAC-объекты.
Secret, созданный отдельно, должен иметь такое же имя с префиксом,
если он используется этой композицией.

ConfigMap общего компонента собирается с `disableNameSuffixHash: true`,
поэтому его имя стабильно и его можно переиспользовать там,
где композиции намеренно используют общий набор настроек.

Не подключайте одновременно несколько компонентов рабочей нагрузки
или несколько компонентов RBAC,
если намеренно не собираете собственную композицию:
они содержат объекты с одинаковыми именами.

## Диагностика

При проблеме сначала проверьте отрендеренный манифест и состояние Job/Pod:

```shell
kubectl kustomize ./kdump
kubectl get pods,jobs -n kube-dump
kubectl describe pod -n kube-dump -l app.kubernetes.io/name=kube-dump
kubectl logs -n kube-dump -l app.kubernetes.io/name=kube-dump --all-containers
```

Проверить фактические права ServiceAccount можно без запуска бекапа:

```shell
kubectl auth can-i --list --as=system:serviceaccount:kube-dump:kube-dump
```

Для `pvc save` отдельно проверьте наличие `VolumeSnapshotClass`,
состояние временных снимков и доступность образа
вспомогательного Pod в реестре кластера.
