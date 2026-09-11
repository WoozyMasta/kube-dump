# Encryption

kube-dump can encrypt:

* individual values inside YAML documents;
* Kubernetes resource archives;
* filesystem archives from PVCs.

AGE and AES-SIV are available for individual YAML fields.
Archives use AGE.

## Key formats

`kube-dump key generate` creates an AGE private key in the native X25519 format.
This is the only format created by the command,
but not the only one accepted by kube-dump.

Supported AGE keys include:

* native AGE X25519: recipients start with `age1`,
  private keys with `AGE-SECRET-KEY-1`;
* SSH Ed25519: public keys start with `ssh-ed25519`;
* SSH RSA: public keys start with `ssh-rsa`.

ECDSA, DSA, and other SSH key types are not suitable.
Ordinary and passphrase-protected Ed25519
and RSA private SSH keys are supported.

An `--identity` file may contain one or more native AGE private keys.
Put an SSH key in a separate PEM or OpenSSH private-key file.
Supply its passphrase with `--identity-passphrase` or,
preferably, `--identity-passphrase-file`.

## AGE

AGE encrypts with a recipient's public key
and decrypts with the corresponding private key.

Create an AGE private key:

```shell
kube-dump key generate --output age-identity.txt
```

Only the public key is needed to create a backup:

```shell
kube-dump resource save dir ./backup --recipient age1...
```

Provide the private key to read it:

```shell
kube-dump resource cat dir ./backup --identity ./age-identity.txt
```

If the private key is protected,
provide its passphrase with `--identity-passphrase`
or use the safer `--identity-passphrase-file`.

> [!TIP]
> AGE allows the backup producer to have no ability to decrypt the backup.

AGE produces a new ciphertext on every encryption.
A file changes even when the source value does not.
This is normal for ordinary backups,
but can be unpleasant for YAML stored in Git.

## AES-SIV

AES-SIV provides deterministic encryption.
With the same plaintext and key, the result does not change.
This is useful in Git
because an unchanged Secret does not create a diff on every run.

The trade-off is more involved key management.
kube-dump stores the AES-SIV key in a keyring protected by AGE.

Saving with AES-SIV requires an AGE recipient
and a private key able to open the existing keyring:

```shell
kube-dump resource save dir ./backup \
  --field-encryption aes-siv \
  --recipient age1... \
  --identity ./age-identity.txt
```

## Choosing a mode

Choose AGE when the backup producer must not receive a private decryption key.
Only a public recipient is needed to create the backup;
the private key is needed when reading or decrypting it.
This is the simple choice for archives, backup exchange,
and most storage scenarios.

Choose AES-SIV for YAML in Git when avoiding unnecessary diffs matters.
The ciphertext remains stable for unchanged values and the same key.

> [!WARNING]
> AES-SIV does not replace AGE.
> AES-SIV keys are stored in an AGE-protected keyring.
> Every run with `--field-encryption aes-siv` needs a private AGE
> or compatible SSH key that can open that keyring.

## Encrypting individual fields

Mark fields for encryption in the profile:

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

Then select the encryption mode at runtime. For AGE:

```shell
kube-dump resource save dir ./backup --field-encryption age --recipient age1...
```

The encrypted value is marked with an AGE tag:

```yaml
password: !kube-dump/age |-
  ...
```

AES-SIV uses a different tag:

```yaml
password: !kube-dump/aes-siv |-
  ...
```

`resource cat` decrypts these tags and writes ordinary YAML:

```shell
kube-dump resource cat dir ./backup --identity ./age-identity.txt |\
  kubectl apply -f -
```

## Decrypting for editing

Decrypt a file tree, edit the values, and encrypt it again:

```shell
kube-dump resource decrypt dir ./backup ./backup-decrypted \
  --identity ./age-identity.txt
```

kube-dump preserves special tags:

```yaml
password: !kube-dump/age-decrypted MySecretString
password: !kube-dump/aes-siv-decrypted MySecretString
```

They preserve the method used for each field and do not depend on the profile,
so the decrypted fields can be encrypted again:

```shell
kube-dump resource encrypt dir ./backup-decrypted ./backup-encrypted \
  --recipient age1... --identity ./age-identity.txt
```

Directory encryption and decryption replace only the `resources/` subtree.
Other top-level entries are left untouched;
the private `.kube-dump/crypto/` keyring may be updated.
The two managed trees are published as one unit.
If a transform is interrupted, the next resource writer
for the same destination recovers it automatically.
If the input has no keyring, a stale output keyring is removed.
The resource ownership manifest is copied unchanged
because it is used by later `--prune` saves.

## Encrypted resource archives

Create an AGE-encrypted archive directly:

```shell
kube-dump resource save archive ./backup.tar.zst.age \
  --format tar.zst.age --archive-recipient age1...
```

Encrypt an existing archive:

```shell
kube-dump resource encrypt archive ./backup.tar.zst ./backup.tar.zst.age \
  --recipient age1...
```

Decrypt it:

```shell
kube-dump resource decrypt archive ./backup.tar.zst.age ./backup.tar.zst \
  --identity ./age-identity.txt
```

AES-SIV is not used for complete archives.

## Encrypted PVC archives

```shell
kube-dump pvc save dir ./backup --recipient age1...
```

Encrypt an existing PVC archive:

```shell
kube-dump pvc encrypt ./data.tar.zst ./data.tar.zst.age --recipient age1...
```

Decrypt it:

```shell
kube-dump pvc decrypt ./data.tar.zst.age ./data.tar.zst \
  --identity ./age-identity.txt
```

## Multiple recipients

Specify multiple public keys:

```shell
kube-dump resource save dir ./backup \
  --recipient age1first... --recipient age1second...
```

Or provide a recipients file:

```shell
kube-dump resource save dir ./backup --recipients-file ./recipients.txt
```

The file contains one recipient per line.
Empty lines and lines beginning with `#` are ignored.
Native AGE recipients and SSH recipients may be mixed:

```text
# Native AGE recipient
age1example...

# SSH recipients
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ...
```

An ordinary comment may follow an SSH public key.

> [!IMPORTANT]
> `--recipients-file` contains public keys only
> and can be stored with configuration or in a repository.
> Never put private keys there.

Recipients can also be loaded from an HTTP or HTTPS file:

```shell
kube-dump resource save dir ./backup \
  --recipients-url https://keys.example.com/backup.txt
```

The URL must use the same format as `--recipients-file`.
Response size and request time are limited.

> [!WARNING]
> HTTP does not authenticate the source.
> An attacker who intercepts the request can replace the public keys
> and gain access to new encrypted copies.
> Use HTTP only on a trusted network.

For public GitHub SSH keys, use the repeatable option:

```shell
kube-dump resource save dir ./backup \
  --recipient-github-user alice \
  --recipient-github-user bob
```

GitHub keys can change independently of kube-dump.
Old encrypted copies still require the old private key
if a user removes a key from GitHub.

## AES-SIV key maintenance

Create a new active key:

```shell
kube-dump key rotate ./backup \
  --identity ./age-identity.txt --recipient age1...
```

Replace the AGE recipients protecting the keyring:

```shell
kube-dump key rewrap ./backup \
  --identity ./old-age-identity.txt --recipient age1new...
```

These commands maintain the keyring;
they do not automatically rewrite all YAML.
