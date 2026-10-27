package secretredact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPatterns(t *testing.T) {
	cases := map[string]string{"github_token": "gh" + "p_" + strings.Repeat("A", 36), "provider_key": "s" + "k-" + strings.Repeat("b", 40), "aws_access_key": "AK" + "IA" + strings.Repeat("B", 16), "bearer_token": "Bearer " + strings.Repeat("c", 30), "password_assignment": "password=" + "synthetic-pass", "private_key": "-----BEGIN " + "PRIVATE KEY-----\nsynthetic\n-----END " + "PRIVATE KEY-----"}
	for id, input := range cases {
		t.Run(id, func(t *testing.T) {
			c := Counts()
			out := Text(input, c)
			if c[id] != 1 || out == input {
				t.Fatal("missing replacement")
			}
			if Text(out, nil) != out {
				t.Fatal("not idempotent")
			}
		})
	}
}
func TestJSON(t *testing.T) {
	for _, input := range []string{`{"password":"synthetic"}`, `{"password":"synth\u0065tic"}`, `{"nested":[{"api_key":"synthetic"}]}`, `{"tool":"password=synthetic"}`} {
		c := Counts()
		out := JSON([]byte(input), c)
		if !json.Valid(out) || c["password_assignment"] != 1 || strings.Contains(string(out), "synthetic") {
			t.Fatal("unsafe JSON")
		}
		if string(JSON(out, nil)) != string(out) {
			t.Fatal("not idempotent")
		}
	}
}
func TestUnchanged(t *testing.T) {
	s := []byte("{\"number\": 12345678901234567890}\n")
	if string(JSON(s, nil)) != string(s) {
		t.Fatal("modified clean JSON")
	}
}
func BenchmarkLargeTranscript(b *testing.B) {
	s := strings.Repeat(`{"message":"ordinary synthetic prose with no credentials"}`+"\n", 18000)
	b.SetBytes(int64(len(s)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Text(s, nil)
	}
}
