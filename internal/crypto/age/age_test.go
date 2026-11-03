package age

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"golang.org/x/crypto/ssh"
)

func TestAgeRoundTrip(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}

	for _, armored := range []bool{false, true} {
		t.Run(map[bool]string{false: "binary", true: "armor"}[armored], func(t *testing.T) {
			var encrypted bytes.Buffer
			writer, err := NewEncryptWriter(
				&encrypted,
				[]age.Recipient{identity.Recipient()},
				EncryptOptions{Armor: armored},
			)
			if err != nil {
				t.Fatalf("create writer: %v", err)
			}

			if _, err := writer.Write([]byte("secret bytes")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			reader, err := OpenDecryptReader(
				bytes.NewReader(encrypted.Bytes()),
				[]age.Identity{identity},
			)
			if err != nil {
				t.Fatalf("open reader: %v", err)
			}

			plain, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(plain) != "secret bytes" {
				t.Fatalf("unexpected plaintext %q", plain)
			}
		})
	}
}

func TestNewEncryptWriterRejectsInvalidInput(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	if _, err := NewEncryptWriter(nil, []age.Recipient{identity.Recipient()}, EncryptOptions{}); err == nil {
		t.Fatal("expected nil destination error")
	}
	if _, err := NewEncryptWriter(io.Discard, nil, EncryptOptions{}); err == nil {
		t.Fatal("expected missing recipient error")
	}
}

func TestLoadNativeRecipientsAndIdentities(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}

	directory := t.TempDir()
	identityPath := filepath.Join(directory, "identity")
	if err := os.WriteFile(identityPath, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}

	recipientPath := filepath.Join(directory, "recipient")
	if err := os.WriteFile(recipientPath, []byte(identity.Recipient().String()+"\n"), 0o600); err != nil {
		t.Fatalf("write recipient: %v", err)
	}

	identities, err := LoadIdentities([]string{identityPath}, IdentityOptions{})
	if err != nil {
		t.Fatalf("load identities: %v", err)
	}

	recipients, err := LoadRecipients([]string{recipientPath})
	if err != nil {
		t.Fatalf("load recipients: %v", err)
	}
	if len(identities) != 1 || len(recipients) != 1 {
		t.Fatalf("unexpected key counts: identities=%d recipients=%d", len(identities), len(recipients))
	}
}

func TestLoadOpenSSHIdentity(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate SSH key: %v", err)
	}

	block, err := ssh.MarshalPrivateKey(privateKey, "test")
	if err != nil {
		t.Fatalf("marshal SSH key: %v", err)
	}

	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write SSH key: %v", err)
	}

	identities, err := LoadIdentities([]string{path}, IdentityOptions{})
	if err != nil {
		t.Fatalf("load OpenSSH identity: %v", err)
	}
	if len(identities) != 1 {
		t.Fatalf("loaded identities = %d, want 1", len(identities))
	}
}

func TestLoadOpenSSHIdentityWithPassphrase(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate SSH key: %v", err)
	}

	block, err := ssh.MarshalPrivateKeyWithPassphrase(
		privateKey,
		"test",
		[]byte("test-passphrase"),
	)
	if err != nil {
		t.Fatalf("marshal encrypted SSH key: %v", err)
	}

	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write encrypted SSH key: %v", err)
	}

	_, err = LoadIdentities([]string{path}, IdentityOptions{})
	if err == nil {
		t.Fatal("LoadIdentities() accepted an encrypted SSH key without a passphrase")
	}
	if !strings.Contains(err.Error(), "--identity-passphrase") {
		t.Fatalf("LoadIdentities() error = %q, want passphrase guidance", err)
	}

	identities, err := LoadIdentities([]string{path}, IdentityOptions{
		Passphrase: "test-passphrase",
	})
	if err != nil {
		t.Fatalf("load passphrase-protected SSH identity: %v", err)
	}
	if len(identities) != 1 {
		t.Fatalf("loaded identities = %d, want 1", len(identities))
	}
}

func TestLoadOpenSSHIdentityPassphraseFile(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate SSH key: %v", err)
	}

	block, err := ssh.MarshalPrivateKeyWithPassphrase(
		privateKey,
		"test",
		[]byte("test-passphrase"),
	)
	if err != nil {
		t.Fatalf("marshal encrypted SSH key: %v", err)
	}

	directory := t.TempDir()
	identityPath := filepath.Join(directory, "id_ed25519")
	if err := os.WriteFile(identityPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write encrypted SSH key: %v", err)
	}
	passphrasePath := filepath.Join(directory, "passphrase")
	if err := os.WriteFile(passphrasePath, []byte("test-passphrase\r\n"), 0o600); err != nil {
		t.Fatalf("write passphrase: %v", err)
	}

	identities, err := LoadIdentities([]string{identityPath}, IdentityOptions{
		PassphraseFile: passphrasePath,
	})
	if err != nil {
		t.Fatalf("load identity with passphrase file: %v", err)
	}
	if len(identities) != 1 {
		t.Fatalf("loaded identities = %d, want 1", len(identities))
	}
}

func TestLoadIdentitiesRejectsPassphraseSourceConflict(t *testing.T) {
	_, err := LoadIdentities([]string{"identity"}, IdentityOptions{
		Passphrase:     "one",
		PassphraseFile: "two",
	})
	if err == nil {
		t.Fatal("LoadIdentities() accepted conflicting passphrase sources")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("LoadIdentities() error = %q, want conflict guidance", err)
	}
}
