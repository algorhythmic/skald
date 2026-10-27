package secretredact

import (
	"encoding/json"
	"github.com/algorhythmic/skald/internal/testsupport"
	"strings"
	"testing"
)

func TestPatterns(t *testing.T) {
	for _, fixture := range testsupport.Secrets() {
		id, input := fixture.Rule, fixture.Input
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
func TestInspectDuplicateEscapedKeys(t *testing.T) {
	c := Counts()
	Inspect([]byte(`{"password":"synthetic","password":"","nested":{"pass\u0077ord":12345}}`), c)
	if c["password_assignment"] != 2 {
		t.Fatal("duplicate or escaped key missed")
	}
	if Check([]byte(`{"password":"synthetic","password":""}`)) == nil {
		t.Fatal("unsafe duplicate allowed")
	}
}
func TestQuotedPassword(t *testing.T) {
	s := `password="synthetic phrase with spaces"`
	out := Text(s, nil)
	if strings.Contains(out, "phrase") || Text(out, nil) != out {
		t.Fatal("partial quoted replacement")
	}
}
func TestWireRaw(t *testing.T) {
	raw := []byte(`{"password":"synthetic"}`)
	wire, _ := json.Marshal(map[string]any{"raw": raw})
	if CheckWire(wire) == nil {
		t.Fatal("base64 raw bypass")
	}
}

// This measures the added policy cost on one large native capture record, not
// full daemon CPU or disk throughput. Fixture construction is outside the timer.
func BenchmarkCheckLargeCapture(b *testing.B) {
	raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "synthetic", "message": map[string]string{"content": strings.Repeat("ordinary synthetic prose ", 44000)}})
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if Check(raw) != nil {
			b.Fatal("clean synthetic fixture rejected")
		}
	}
}
func TestEnvironmentAndEmbeddedJSON(t *testing.T) {
	for _, input := range []string{`OPENAI_API_KEY=synthetic`, `{"password":"synthetic"}`, `WEBPASSWORD=synthetic`, `password=[REDACTED:not-a-policy-id]`} {
		if Text(input, nil) == input {
			t.Fatal("labeled credential missed")
		}
	}
}
func TestJSONReplacementDuplicateSensitiveKey(t *testing.T) {
	input := []byte(`{"password":"synthetic","password":""}`)
	out := JSON(input, nil)
	if Check(out) != nil || string(out) == string(input) {
		t.Fatal("duplicate sensitive field not handled")
	}
}
func TestFineGrainedGitHubPrefilter(t *testing.T) {
	s := "github" + "_pat_" + strings.Repeat("D", 40)
	if Text(s, nil) == s {
		t.Fatal("prefilter missed token")
	}
}
func TestDepthBoundFailsClosed(t *testing.T) {
	raw := []byte(strings.Repeat("[", 256) + `"ordinary"` + strings.Repeat("]", 256))
	if Check(raw) == nil {
		t.Fatal("deep input accepted")
	}
}
func TestPasswordPlaceholders(t *testing.T) {
	for _, v := range []string{"", "$VAR", "${VALUE}", "<value>", "***", "changeme", "password", "str", "string", "Optional[str]"} {
		input := "password=" + v
		c := Counts()
		Text(input, c)
		if c["password_assignment"] != 0 {
			t.Fatal("placeholder candidate counted")
		}
		raw, _ := json.Marshal(map[string]string{"password": v})
		c = Counts()
		Inspect(raw, c)
		if c["password_assignment"] != 0 {
			t.Fatal("JSON placeholder counted")
		}
	}
}
func TestRealPasswordAssignments(t *testing.T) {
	for _, v := range []string{strings.Repeat("Q", 16), "changeme123", "stringy", "12345"} {
		input := "password=" + v
		c := Counts()
		out := Text(input, c)
		if c["password_assignment"] != 1 || out == input {
			t.Fatal("real assignment missed")
		}
	}
}
func TestTypedRealAssignment(t *testing.T) {
	input := `password: str = "` + strings.Repeat("Q", 18) + `"`
	if Text(input, nil) == input {
		t.Fatal("typed real assignment missed")
	}
}
func TestDuplicateJSONReplacementPreservesObject(t *testing.T) {
	input := []byte(`{"password":"` + strings.Repeat("Q", 18) + `","password":"","other":12345678901234567890}`)
	out := JSON(input, nil)
	if !json.Valid(out) || len(out) == 0 || out[0] != '{' || Check(out) != nil || !strings.Contains(string(out), "12345678901234567890") {
		t.Fatal("duplicate JSON replacement lost object shape")
	}
}

func TestAdditionalRuleVariants(t *testing.T) {
	for _, kind := range []string{"auth", "client", "api", "EXAMPLE"} {
		input := "ts" + "key-" + kind + "-" + strings.Repeat("A", 32)
		counts := Counts()
		out := Text(input, counts)
		if counts["tailscale_key"] != 1 || strings.Contains(out, strings.Repeat("A", 32)) {
			t.Fatal("tailscale variant missed")
		}
	}
	for _, label := range []string{"PrivateKey", "PresharedKey"} {
		value := strings.Repeat("A", 43) + "="
		input := "[Interface]\n" + label + " = " + value + "\n"
		counts := Counts()
		out := Text(input, counts)
		if counts["wireguard_private_key"] != 1 || strings.Contains(out, value) || Text(out, nil) != out {
			t.Fatal("wireguard variant missed")
		}
	}
}

func TestAdditionalRuleFalsePositives(t *testing.T) {
	for _, input := range []string{"ordinary prose mentioning tskey", "PublicKey = " + strings.Repeat("A", 43) + "=", "PVEAuthCookie", "PVEAuthCookie="} {
		counts := Counts()
		if Text(input, counts) != input {
			t.Fatal("ordinary text changed")
		}
		for _, n := range counts {
			if n != 0 {
				t.Fatal("ordinary text counted")
			}
		}
	}
}

func TestAPIKeyHyphenForms(t *testing.T) {
	value := strings.Repeat("Q", 18)
	for _, input := range []string{"api-key: " + value, `"api-key": "` + value + `"`, `{"api-key":"` + value + `"}`} {
		counts := Counts()
		out := JSON([]byte(input), counts)
		if counts["password_assignment"] != 1 || strings.Contains(string(out), value) {
			t.Fatal("api-key form missed")
		}
	}
}

func TestAllRulesMixedMultiline(t *testing.T) {
	var lines []string
	for _, fixture := range testsupport.Secrets() {
		lines = append(lines, fixture.Input)
	}
	input := strings.Join(lines, "\n")
	counts := Counts()
	out := Text(input, counts)
	for _, fixture := range testsupport.Secrets() {
		if counts[fixture.Rule] != 1 || strings.Contains(out, fixture.Value) {
			t.Fatal("mixed multiline replacement missed")
		}
	}
	if Text(out, nil) != out {
		t.Fatal("mixed replacement not idempotent")
	}
	if len(IDs()) != len(testsupport.Secrets()) {
		t.Fatal("shared per-rule matrix incomplete")
	}
}
