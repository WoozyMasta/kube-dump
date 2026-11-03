# 运维场景

本页介绍组织和运行 kube-dump 备份时的实用建议。

## 多个配置文件

同一个集群可以使用不同的配置文件，并设置不同的运行计划。例如，
一个配置文件选择 `dev-*` 命名空间并频繁运行，另一个选择 `test-*` 命名空间
并降低运行频率。

两个配置文件可以使用同一个目录：

```shell
kube-dump resource save dir ./backup --profile ./profiles/dev.yaml
kube-dump pvc save dir ./backup --profile ./profiles/dev.yaml
kube-dump image save dir ./backup --profile ./profiles/dev.yaml
```

对于 `test` 配置文件，只需要替换配置文件路径：

```shell
kube-dump resource save dir ./backup --profile ./profiles/test.yaml
kube-dump pvc save dir ./backup --profile ./profiles/test.yaml
kube-dump image save dir ./backup --profile ./profiles/test.yaml
```

不使用 `--prune` 时，这些运行会向现有结果中追加内容。某个配置文件没有
选中某些资源，并不会因此删除另一个配置文件保存的资源。来自不同命名空间
的 PVC 也会保存到不同的子目录中。

如果相互独立的计划任务可能同时运行，请遵循[并发运行][]中的规则。

> [!CAUTION]
> 不要让使用同一目录的独立任务同时使用 `--prune`。
> 后执行的 `dev` 任务可能会删除 `test` 任务保存的结果。

## 多个集群

通常应为每个集群分配独立目录。这是最简单的方式：不同集群的数据不会混在
一起，三类备份都位于对应集群的根目录下。

```text
backup/
├── cluster-a/
│   ├── resources/
│   ├── volumes/
│   └── images/
└── cluster-b/
    ├── resources/
    ├── volumes/
    └── images/
```

`cluster-a` 的示例命令：

```shell
kube-dump resource save dir ./backup/cluster-a
kube-dump pvc save dir ./backup/cluster-a
kube-dump image save dir ./backup/cluster-a
```

如果多个集群使用相同的镜像，可以将镜像目录移到共享根目录。OCI layout
按 digest 保存内容，因此相同的层会被复用，不会重复占用磁盘空间。

```text
backup/
├── cluster-a/
│   ├── resources/
│   └── volumes/
├── cluster-b/
│   ├── resources/
│   └── volumes/
└── images/
```

> [!CAUTION]
> 共享根目录只能用于镜像。不要把不同集群的资源或 PVC 放在那里，
> 即使当前每个配置文件选择的是不同命名空间。命令可能仍然成功，
> 但集群级资源、同名命名空间或 PVC，以及未来的配置变更，都可能导致数据被覆盖。
> 命名空间不是集群之间的隔离边界。

在这种布局中，镜像命令使用 `./backup`：

```shell
kube-dump image save dir ./backup
```

S3 也遵循同样的原则：资源和 PVC 使用不同前缀，镜像使用共享前缀。
如果集群分开保存，不要把不同集群的资源或 PVC 混在同一个前缀下。

如果不同集群的任务可能重叠运行，请遵循[并发运行][]中的规则。

## 并发运行 {#concurrent-runs}

如果两个进程同时写入同一个目录或 S3 前缀，结果取决于数据类型。

* 对于本地镜像，第二个 `image save dir` 会等待第一个任务完成，
  从而避免同时修改共享 OCI layout。
* 资源保存也使用相同类型的写入锁。不同资源目录可以并行写入。
* 如果选择的 PVC 集合不重叠，可以并行保存不同 PVC。不要同时运行针对同一个
  PVC 的两个任务，因为它们可能同时修改相同文件。
* 不同机器之间没有共享的文件锁。对于 S3 `image save`，kube-dump 会读取当前
  index，并在发布时检查它是否发生变化。如果另一个进程先保存了更改，
  后一个任务会因冲突失败，而不会覆盖已经发布的 index。
* `image save s3` 会把引用合并到共享 OCI index 中，但不会删除旧 blobs。

本地锁使用空的标记文件，文件名为
`.kube-dump-resource-writer-*.lock` 和 `.kube-dump-image-writer-*.lock`。
命令完成后，这些文件仍会保留在父目录中；实际锁由文件描述符持有，
关闭文件时就会释放。如果没有正在使用这些目标的 `kube-dump` 进程，
可以手动删除这些标记文件。

> [!NOTE]
> `.lock` 文件存在并不表示目标当前处于锁定状态。
> 不要在 `kube-dump` 运行期间删除它，
> 否则可能破坏并发写入保护。

## 清理旧结果

在 `resource save` 和本地 `image save` 中，`--prune` 会启用破坏性清理。
成功保存后，kube-dump 会删除当前选择结果中不存在的内容。

对于 `resource save s3`，结果同样根据当前选择构建，但旧版本会延后删除：
当前版本和前一个版本会为读取者保留。

这样可以清理由于集群中对象被删除而在上一次运行后遗留的数据。

> [!CAUTION]
> 只有在一个备份使用稳定且非动态的配置时才使用 `--prune`。
> 不应有其他配置文件、集群或计划任务写入目标目录，命名空间和选择器也不能
> 在不同运行之间改变。否则，后续运行会删除不属于当前选择结果的内容。

如果自定义文件的生命周期与备份不同，不要把它们放在受管理的
`resources/`、`volumes/` 或 `images/` 子目录中。请使用相邻的顶层目录保存自定义文件。

[并发运行]: #concurrent-runs
