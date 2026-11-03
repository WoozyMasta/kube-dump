# Inline encryption fixtures

The files in this directory are immutable contract fixtures
for inline resource-field encryption.

Do not edit, regenerate, or replace existing files.
A changed fixture means that the serialized field-encryption contract changed
and requires explicit review.

The age fixture is checked by decrypting it and by encrypting plaintext again.
Its ciphertext is not compared byte-for-byte because age uses fresh randomness.
The AES-SIV fixture is deterministic and is compared exactly.
