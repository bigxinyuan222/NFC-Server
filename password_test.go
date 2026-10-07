package main

import "testing"

func TestPasswordHash(t *testing.T) {
	hash, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashPassword() error = %v", err)
	}
	if !verifyPassword(hash, "correct horse battery staple") {
		t.Fatal("verifyPassword() rejected the original password")
	}
	if verifyPassword(hash, "wrong password") {
		t.Fatal("verifyPassword() accepted a wrong password")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, hash := range []string{"", "plain-text", "pbkdf2_sha256$1$salt$hash"} {
		if verifyPassword(hash, "password") {
			t.Fatalf("verifyPassword() accepted malformed hash %q", hash)
		}
	}
}
