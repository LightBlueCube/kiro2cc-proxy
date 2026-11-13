package control

import (
	"strings"
	"testing"
)

func TestRefreshTokenFrom(t *testing.T) {
	const token = "1//0gExampleRefreshToken-_abc"
	cases := []struct {
		name, pasted, want, err string
	}{
		{"bare token", "  " + token + "\n", token, ""},
		{"agy token file", `{"token":{"access_token":"ya29.x","refresh_token":"` + token + `"},"auth_method":"consumer"}`, token, ""},
		{"camelCase export", `{"email":"a@b.c","refreshToken":"` + token + `"}`, token, ""},
		{"array of one", `[{"refresh_token":"` + token + `"}]`, token, ""},
		{"same token twice", `{"refresh_token":"` + token + `","token":{"refresh_token":"` + token + `"}}`, token, ""},
		{"two accounts", `[{"refresh_token":"` + token + `"},{"refresh_token":"1//other"}]`, "", "2 refresh tokens"},
		{"no refresh token", `{"access_token":"ya29.x"}`, "", "no refresh_token"},
		{"bad JSON", `{"refresh_token":`, "", "unreadable JSON"},
		{"access token", "ya29.a0Example", "", "access token"},
		{"not a token", "hello world", "", "not a Google refresh token"},
		{"empty value", `{"refresh_token":""}`, "", "not a Google refresh token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := refreshTokenFrom(c.pasted)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want one containing %q", err, c.err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}
