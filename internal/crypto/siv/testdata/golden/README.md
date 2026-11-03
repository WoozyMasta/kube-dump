# AES-SIV contract fixtures

The files in this directory are an immutable AES-256-SIV contract vector
for the `internal/crypto/siv` package.

Do not edit, regenerate, or replace existing files.
A change means that the ciphertext or serialized payload contract changed
and requires explicit review.

The vector fixes the key, AAD, plaintext, ciphertext, and encoded payload.
The test checks both encryption and decryption,
including authentication failure after changing the AAD or ciphertext.
