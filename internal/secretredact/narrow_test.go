package secretredact

import (
	"encoding/json"
	"strings"
	"testing"
)

// Shape-only synthetic corpus; no archive examples or values are copied here.
func falsePositiveShapes() []string {
	return []string{
		"token=token", "token=self.token", "password=args.password", "key=os.environ[NAME]",
		"token=access_token", "password=password", "secret=secret", "api_key=config.api_key",
		"token=settings.token", "password=credentials.password", "token=request.headers.get(NAME)",
		"password=get_password()", "token=make_token()", "client(token=token)", "client(password=args.password)",
		"client(api_key=config.key)", "token: str", "password: Optional[str] = None", "token: string",
		"password: str = None", "token=None", "password=True", "token=False", "password=$PASSWORD",
		"token=${TOKEN}", "password=<value>", "password=***", "password=changeme",
		"token=fixture_token", "password=fixture.password", "token=example", "The word token appears in prose.", "password=12345678",
	}
}
func positiveLiteralShapes() []string {
	var out []string
	for _, label := range []string{"password", "passwd", "token", "secret", "api-key", "aws_secret_access_key"} {
		for _, quote := range []string{"\"", "'"} {
			out = append(out, label+"="+quote+strings.Repeat("Q", 18)+quote)
		}
		out = append(out, label+"="+"9aB2cD3e"+"F4gH5iJ6")
	}
	return out
}
func TestNarrowSyntheticCounts(t *testing.T) {
	fp, positive := 0, 0
	for _, input := range falsePositiveShapes() {
		if Text(input, nil) != input {
			fp++
		}
	}
	for _, input := range positiveLiteralShapes() {
		if Text(input, nil) != input {
			positive++
		}
	}
	t.Logf("false_positive_shapes=%d matched=%d positive_shapes=%d matched=%d", len(falsePositiveShapes()), fp, len(positiveLiteralShapes()), positive)
	if fp != 0 || positive != len(positiveLiteralShapes()) {
		t.Fatal("literal policy mismatch")
	}
	for _, input := range positiveLiteralShapes() {
		out := Text(input, nil)
		if Text(out, nil) != out {
			t.Fatal("literal replacement not idempotent")
		}
	}
}

func TestBareTokenHeuristic(t *testing.T) {
	for _, value := range []string{"9aB2cD3e", "9aB2cD3eF4gH5iJ6", "aB2cD3eF+/="} {
		if !literalAssignment(value) {
			t.Fatal("high entropy token missed")
		}
	}
	for _, value := range []string{"9aB2cD3", "aaaaaaaa-1", "1234567890", "self.token", "args.password", "VARIABLE_NAME", "aB2cD3eF", "get_token()"} {
		if literalAssignment(value) {
			t.Fatal("reference or low entropy value matched")
		}
	}
}

func TestReferenceShapesInJSONText(t *testing.T) {
	for _, input := range falsePositiveShapes() {
		raw, _ := json.Marshal(map[string]string{"content": input})
		if string(JSON(raw, nil)) != string(raw) {
			t.Fatal("reference text changed inside JSON")
		}
	}
}
func TestJSONLiteralValueTypes(t *testing.T) {
	for _, raw := range []string{`{"password":12345678}`, `{"token":null}`, `{"token":false}`} {
		counts := Counts()
		out := JSON([]byte(raw), counts)
		if string(out) != raw || counts["password_assignment"] != 0 {
			t.Fatal("non-string JSON literal changed")
		}
	}
	// A JSON string is an explicit literal, even if its spelling resembles code.
	raw, _ := json.Marshal(map[string]string{"password": "args.password"})
	counts := Counts()
	out := JSON(raw, counts)
	if counts["password_assignment"] != 1 || string(out) == string(raw) {
		t.Fatal("quoted JSON literal missed")
	}
}

func TestQuotedLiteralInEmbeddedJSONProjection(t *testing.T) {
	value := strings.Repeat("Q", 18)
	inner, _ := json.Marshal(map[string]string{"text": "password=\"" + value + "\""})
	wire, _ := json.Marshal(map[string]string{"text": string(inner)})
	out, err := Wire(wire)
	if err != nil || strings.Contains(string(out), value) || CheckWire(out) != nil {
		t.Fatal("nested projection literal escaped redaction")
	}
}
