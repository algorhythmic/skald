// Package testsupport assembles synthetic detector fixtures at runtime. No
// credential literal or real credential is present in the repository.
package testsupport

import "strings"

type Secret struct{ Rule, Input, Value string }

func Secrets() []Secret {
	gh := "gh" + "p_" + strings.Repeat("A", 36)
	provider := "s" + "k-" + strings.Repeat("B", 40)
	aws := "AK" + "IA" + strings.Repeat("C", 16)
	bearer := strings.Repeat("D", 32)
	password := strings.Repeat("Q", 18)
	private := strings.Join([]string{"-----BEGIN ", "PRIVATE KEY-----\n", strings.Repeat("Z", 64), "\n-----END ", "PRIVATE KEY-----"}, "")
	return []Secret{{"github_token", gh, gh}, {"provider_key", provider, provider}, {"aws_access_key", aws, aws}, {"bearer_token", "Bearer " + bearer, bearer}, {"password_assignment", "password=" + password, password}, {"private_key", private, private}}
}
