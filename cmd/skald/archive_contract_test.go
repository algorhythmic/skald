package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestArchiveContractCommandsRejectInvalidFlagsBeforeTransport(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"changes", "--socket", "/tmp/skald-absent.sock", "--scope", "record"}, "flag_not_supported_for_command"},
		{[]string{"capabilities", "--socket", "/tmp/skald-absent.sock", "--limit", "2"}, "flag_not_supported_for_command"},
		{[]string{"redact", "--socket", "/tmp/skald-absent.sock", "--namespace", "fixture:claude", "--scope", "record"}, "namespace_scope_and_target_required"},
		{[]string{"redact", "--socket", "/tmp/skald-absent.sock", "--namespace", "fixture:claude", "--target", "record"}, "namespace_scope_and_target_required"},
	} {
		var out, stderr bytes.Buffer
		err := runArchive(tc.args, &out, &stderr)
		if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
			t.Fatalf("%v: err=%v out=%s", tc.args, err, out.String())
		}
	}
}
