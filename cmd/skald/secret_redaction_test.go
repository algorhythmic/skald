package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSensitiveCLIOutputSanitized(t *testing.T) {
	var out bytes.Buffer
	e := writeJSON(&out, map[string]string{"password": "synthetic"})
	if e != nil || !strings.Contains(out.String(), "[REDACTED:") || strings.Contains(out.String(), "synthetic") {
		t.Fatal("sensitive CLI output escaped")
	}
}
