package main

import (
	"bytes"
	"testing"
)

func TestSensitiveCLIOutputWithheld(t *testing.T) {
	var out bytes.Buffer
	e := writeJSON(&out, map[string]string{"password": "synthetic"})
	if e == nil || out.Len() != 0 {
		t.Fatal("sensitive CLI output escaped")
	}
}
