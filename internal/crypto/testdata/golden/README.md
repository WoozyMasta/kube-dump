# Cryptographic compatibility fixtures

The files in this directory are immutable compatibility fixtures
for formats produced by `kube-dump`.

Do not edit, regenerate, or replace existing files.
A change means that the compatibility contract changed
and requires explicit review.

Age encryption uses fresh randomness, so tests decrypt its fixtures
instead of comparing newly generated ciphertext byte-for-byte.
AES-SIV is deterministic for the same key, AAD, and plaintext,
so its vector is compared exactly.

The identities and keys are test-only material
and must never be used for real data.
