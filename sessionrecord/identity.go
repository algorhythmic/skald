// Package sessionrecord defines the provider-neutral, versioned capture contract.
// It has no application storage or task-state dependencies.
package sessionrecord

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
)

// Canonical encodes a domain and its ordered UTF-8 fields. Every field, including
// the domain, has an unsigned 32-bit big-endian byte length. No Unicode, path,
// whitespace or case normalization takes place here.
func Canonical(domain string, fields ...string) []byte {
	var b bytes.Buffer
	b.WriteString("sessionrecord\x00v1\x00")
	for _, s := range append([]string{domain}, fields...) {
		if uint64(len(s)) > uint64(^uint32(0)) {
			panic("identity field exceeds uint32")
		}
		_ = binary.Write(&b, binary.BigEndian, uint32(len(s)))
		b.WriteString(s)
	}
	return b.Bytes()
}

func Key(domain string, fields ...string) string {
	sum := sha256.Sum256(Canonical(domain, fields...))
	return "sr1:" + domain + ":" + hex.EncodeToString(sum[:])
}

func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Namespace requires an already canonical, absolute source root and a stable
// nonsecret host identity. Consumers must persist and share the result. Relocation
// is an explicit alias of this namespace, never an automatic identity change.
func Namespace(provider, host, canonicalRoot, profile, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if provider == "" || host == "" || canonicalRoot == "" {
		return "", fmt.Errorf("provider, stable host identity and canonical root required")
	}
	return Key("namespace", provider, host, canonicalRoot, profile), nil
}

func Conversation(namespace, nativeID string) string {
	return Key("conversation", namespace, nativeID)
}

func NativeRecord(scope Scope, nativeID string) string {
	return Key("record", scope.Kind, scope.Key, "native", nativeID)
}

func OrdinalRecord(scope Scope, generation string, ordinal int64) string {
	return Key("record", scope.Kind, scope.Key, "ordinal", generation, strconv.FormatInt(ordinal, 10))
}
