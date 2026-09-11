# 加密

kube-dump 可以加密：

* YAML 文档中的单独值；
* Kubernetes 资源归档；
* PVC 文件系统归档。

单独的 YAML 字段可以使用 AGE 或 AES-SIV。归档使用 AGE。

## 密钥格式

`kube-dump key generate` 会创建 native X25519 格式的 AGE 私钥。
这是该命令创建的唯一格式，但不是 kube-dump 唯一接受的格式。

支持的 AGE 密钥包括：

* native AGE X25519：接收者以 `age1` 开头，私钥以 `AGE-SECRET-KEY-1` 开头；
* SSH Ed25519：公钥以 `ssh-ed25519` 开头；
* SSH RSA：公钥以 `ssh-rsa` 开头。

ECDSA、DSA 和其他 SSH 密钥类型不适用。普通的以及受密码保护的 Ed25519、RSA
SSH 私钥都支持。

一个 `--identity` 文件可以包含一个或多个 native AGE 私钥。SSH 密钥应放在单独
的 PEM 或 OpenSSH 私钥文件中。使用 `--identity-passphrase` 或更推荐的
`--identity-passphrase-file` 提供其密码。

## AGE

AGE 使用接收者公钥加密，并使用对应的私钥解密。

创建 AGE 私钥：

```shell
kube-dump key generate --output age-identity.txt
```

创建备份只需要公钥：

```shell
kube-dump resource save dir ./backup --recipient age1...
```

读取备份时提供私钥：

```shell
kube-dump resource cat dir ./backup --identity ./age-identity.txt
```

如果私钥受保护，请使用 `--identity-passphrase` 或更安全的
`--identity-passphrase-file` 提供密码。

> [!TIP]
> AGE 允许备份创建者不具备解密备份的能力。

AGE 每次加密都会生成新的密文。即使源值没有变化，文件也会变化。
普通备份中这是正常的，但对于保存在 Git 中的 YAML 可能会产生不必要的 diff。

## AES-SIV

AES-SIV 提供确定性加密。对于相同的明文和密钥，结果不会变化。
这对 Git 很有用，因为未变化的 Secret 不会在每次运行时都产生 diff。

代价是密钥管理更复杂。kube-dump 将 AES-SIV 密钥保存在由 AGE 保护的 keyring 中。

使用 AES-SIV 保存时，需要 AGE 接收者和能够打开现有 keyring 的私钥：

```shell
kube-dump resource save dir ./backup \
  --field-encryption aes-siv \
  --recipient age1... \
  --identity ./age-identity.txt
```

## 如何选择模式

如果备份创建者不应获得私有解密密钥，请选择 AGE。创建备份只需要公钥接收者；
读取或解密备份时才需要私钥。这是归档、备份交换和大多数存储场景的简单选择。

如果希望避免 Git 中不必要的 diff，请为 Git 中的 YAML 选择 AES-SIV。
源值未变化时，密文和密钥也保持稳定。

> [!WARNING]
> AES-SIV 不能替代 AGE。AES-SIV 密钥存储在由 AGE 保护的 keyring 中。
> 每次运行 `--field-encryption aes-siv` 都需要能够打开该 keyring 的 AGE 私钥
> 或兼容的 SSH 私钥。

## 加密单独字段

在配置文件中标记要加密的字段：

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

然后在运行时选择加密模式。AGE：

```shell
kube-dump resource save dir ./backup --field-encryption age --recipient age1...
```

加密值会带有 AGE 标签：

```yaml
password: !kube-dump/age |-
  ...
```

AES-SIV 使用不同的标签：

```yaml
password: !kube-dump/aes-siv |-
  ...
```

`resource cat` 会解密这些标签并写出普通 YAML：

```shell
kube-dump resource cat dir ./backup --identity ./age-identity.txt |\
  kubectl apply -f -
```

## 解密后编辑

解密文件树，编辑值，然后再次加密：

```shell
kube-dump resource decrypt dir ./backup ./backup-decrypted \
  --identity ./age-identity.txt
```

kube-dump 会保留特殊标签：

```yaml
password: !kube-dump/age-decrypted MySecretString
password: !kube-dump/aes-siv-decrypted MySecretString
```

标签保留每个字段使用的方法，不依赖配置文件，因此解密后的字段可以再次加密：

```shell
kube-dump resource encrypt dir ./backup-decrypted ./backup-encrypted \
  --recipient age1... --identity ./age-identity.txt
```

目录加密和解密只替换 `resources/` 子目录，其他顶层内容保持不变；
私有密钥环 `.kube-dump/crypto/` 可能会更新。
这两个受管理的目录会作为一个整体发布。如果转换被中断，
同一目标的下一次资源写入操作会自动恢复它。如果输入中没有密钥环，
输出中的过期密钥环也会被删除。
资源所有权清单会原样复制，因为后续使用 `--prune` 保存时需要它。

## 加密资源归档

直接创建 AGE 加密归档：

```shell
kube-dump resource save archive ./backup.tar.zst.age \
  --format tar.zst.age --archive-recipient age1...
```

加密现有归档：

```shell
kube-dump resource encrypt archive ./backup.tar.zst ./backup.tar.zst.age \
  --recipient age1...
```

解密归档：

```shell
kube-dump resource decrypt archive ./backup.tar.zst.age ./backup.tar.zst \
  --identity ./age-identity.txt
```

完整归档不使用 AES-SIV。

## 加密 PVC 归档

```shell
kube-dump pvc save dir ./backup --recipient age1...
```

加密现有 PVC 归档：

```shell
kube-dump pvc encrypt ./data.tar.zst ./data.tar.zst.age --recipient age1...
```

解密归档：

```shell
kube-dump pvc decrypt ./data.tar.zst.age ./data.tar.zst \
  --identity ./age-identity.txt
```

## 多个接收者

指定多个公钥：

```shell
kube-dump resource save dir ./backup \
  --recipient age1first... --recipient age1second...
```

或者提供接收者文件：

```shell
kube-dump resource save dir ./backup --recipients-file ./recipients.txt
```

文件每行包含一个接收者。空行和以 `#` 开头的行会被忽略。
可以混合 native AGE 接收者和 SSH 接收者：

```text
# Native AGE recipient
age1example...

# SSH recipients
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ...
```

SSH 公钥后面可以有普通注释。

> [!IMPORTANT]
> `--recipients-file` 只包含公钥，可以与配置一起存储或放入仓库。
> 不要在其中放入私钥。

也可以从 HTTP 或 HTTPS 文件加载接收者：

```shell
kube-dump resource save dir ./backup \
  --recipients-url https://keys.example.com/backup.txt
```

URL 必须使用与 `--recipients-file` 相同的格式。响应大小和请求时间受到限制。

> [!WARNING]
> HTTP 不会验证来源。拦截请求的攻击者可以替换公钥，从而获得新加密副本的访问权。
> 只在可信网络中使用 HTTP。

对于 GitHub SSH 公钥，请使用可重复的选项：

```shell
kube-dump resource save dir ./backup \
  --recipient-github-user alice \
  --recipient-github-user bob
```

GitHub 上的密钥可能独立于 kube-dump 发生变化。如果用户从 GitHub 删除密钥，
旧的加密副本仍需要旧私钥。

## AES-SIV 密钥维护

创建新的活动密钥：

```shell
kube-dump key rotate ./backup \
  --identity ./age-identity.txt --recipient age1...
```

替换保护 keyring 的 AGE 接收者：

```shell
kube-dump key rewrap ./backup \
  --identity ./old-age-identity.txt --recipient age1new...
```

这些命令只维护 keyring，不会自动重写所有 YAML。
