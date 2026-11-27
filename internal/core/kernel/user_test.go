package kernel

import (
	"encoding/json"
	"strings"
	"testing"
)

// The password hash and avatar version are exported for package core, and
// never leave in JSON.
func TestUserJSONKeepsSecretsOut(t *testing.T) {
	b, err := json.Marshal(&User{ID: 1, Username: "robin", PasswordHash: "$argon2id$secret", AvatarVersion: 424242})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"argon2id", "424242", "PasswordHash", "AvatarVersion", "password", "avatar_version"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("%q in %s", leak, b)
		}
	}
}
