package sessioncapture

import (
	"bufio"
	"encoding/json"
	"errors"
	"github.com/algorhythmic/skald/internal/secretredact"
	"io"
	"strings"
)

const privateConversationPrefix = "[REDACTED:checkpoint_identity]:"

// PublicConversation substitutes a stream-local identity, never a fingerprint of
// the sensitive ID. Native continuity is checked in memory against the verified
// source prefix; the archive only receives this marker and existing stream key.
func PublicConversation(sourceKey, id string) string {
	if strings.HasPrefix(id, privateConversationPrefix) {
		return privateConversationPrefix + sourceKey
	}
	if secretredact.Text(id, nil) != id {
		return privateConversationPrefix + sourceKey
	}
	return id
}
func PublicCheckpoint(cp Checkpoint) Checkpoint {
	cp.ConversationID = PublicConversation(cp.SourceKey, cp.ConversationID)
	cp.ProviderVersion = secretredact.Text(cp.ProviderVersion, nil)
	return cp
}
func PublicSource(s Source) Source {
	s.ConversationID = PublicConversation(s.Key(), s.ConversationID)
	s.ProviderVersion = secretredact.Text(s.ProviderVersion, nil)
	return s
}

// Recover only the first bounded native identity record. A source without a
// configured conversation could not have accepted its first line without this
// identity. Codex fork roots are already bounded/probed by read(). The native
// prefix is still verified before any recovered identity can be committed.
func restoreNativeConversation(input io.ReadSeeker, source Source, cp Checkpoint, codex codexForkContext, limit int) (string, error) {
	if !strings.HasPrefix(cp.ConversationID, privateConversationPrefix) {
		return cp.ConversationID, nil
	}
	if source.ConversationID != "" && !strings.HasPrefix(source.ConversationID, privateConversationPrefix) {
		return source.ConversationID, nil
	}
	if codex.root != "" {
		return codex.root, nil
	}
	position, e := input.Seek(0, io.SeekCurrent)
	if e != nil {
		return "", e
	}
	defer input.Seek(position, io.SeekStart)
	if _, e = input.Seek(0, io.SeekStart); e != nil {
		return "", e
	}
	line, complete, large, e := readLine(bufio.NewReaderSize(input, 64<<10), limit)
	if e != nil || !complete || large {
		return "", errors.New("checkpoint_identity_unavailable")
	}
	var native object
	if json.Unmarshal(line, &native) != nil {
		return "", errors.New("checkpoint_identity_unavailable")
	}
	if source.Provider == Claude {
		id, e := claudeIdentity(native)
		if e != nil {
			return "", e
		}
		if id.conversationID() != "" {
			return id.conversationID(), nil
		}
	}
	return "", errors.New("checkpoint_identity_unavailable")
}
