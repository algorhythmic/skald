package sessioncapture

import (
	"crypto/sha256"
	"encoding"
	"hash"
)

// Keep pending source bytes out of serialized hints. The underlying hash only
// ever receives complete blocks; its serialized buffer therefore stays empty.
// At restore, at most63 boundary bytes are reread from the source.
type prefixHash struct {
	hash.Hash
	pending []byte
}

func newPrefixHash() *prefixHash { return &prefixHash{Hash: sha256.New()} }
func (h *prefixHash) Write(p []byte) (int, error) {
	n := len(p)
	if len(h.pending) > 0 {
		take := min(sha256.BlockSize-len(h.pending), len(p))
		h.pending = append(h.pending, p[:take]...)
		p = p[take:]
		if len(h.pending) == sha256.BlockSize {
			_, _ = h.Hash.Write(h.pending)
			h.pending = nil
		}
	}
	blocks := len(p) / sha256.BlockSize * sha256.BlockSize
	if blocks > 0 {
		_, _ = h.Hash.Write(p[:blocks])
		p = p[blocks:]
	}
	h.pending = append(h.pending, p...)
	return n, nil
}
func (h *prefixHash) Sum(b []byte) []byte {
	state, _ := h.Hash.(encoding.BinaryMarshaler).MarshalBinary()
	cloned := sha256.New()
	_ = cloned.(encoding.BinaryUnmarshaler).UnmarshalBinary(state)
	_, _ = cloned.Write(h.pending)
	return cloned.Sum(b)
}
func (h *prefixHash) Reset() { h.Hash.Reset(); h.pending = nil }
func (h *prefixHash) MarshalBinary() ([]byte, error) {
	return h.Hash.(encoding.BinaryMarshaler).MarshalBinary()
}
func (h *prefixHash) UnmarshalBinary(b []byte) error {
	h.pending = nil
	return h.Hash.(encoding.BinaryUnmarshaler).UnmarshalBinary(b)
}
