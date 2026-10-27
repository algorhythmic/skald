package main

import (
	"bytes"
	"testing"
)

func TestMaintenanceJobDefaultsToPreview(t *testing.T) {
	var out, stderr bytes.Buffer
	if e := runMaintenance([]string{"recovery-job", "--destination-dir", "/unconfigured/independent"}, &out, &stderr); e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"applied": false`)) {
		t.Fatal(out.String())
	}
}
func TestMaintenanceValidation(t *testing.T) {
	for _, args := range [][]string{{"recovery-retain", "--keep-raw", "0"}, {"storage-usage"}, {"recovery-job"}, {"recovery-check", "--apply"}} {
		var out bytes.Buffer
		if e := runMaintenance(args, &out, &out); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
