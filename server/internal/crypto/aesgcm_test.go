package crypto

import "testing"

func TestEncryptDecryptRoundTrip(t *testing.T) {
	const secret = "test-secret-key-material"
	cases := []string{
		"sk-1234567890abcdef",
		"a",
		"another upstream bearer token with spaces and symbols !@#$%^&*()",
	}
	for _, plain := range cases {
		enc, err := Encrypt(secret, plain)
		if err != nil {
			t.Fatalf("Encrypt(%q) error: %v", plain, err)
		}
		if enc == plain {
			t.Fatalf("ciphertext equals plaintext for %q (not encrypted)", plain)
		}
		dec, err := Decrypt(secret, enc)
		if err != nil {
			t.Fatalf("Decrypt error: %v", err)
		}
		if dec != plain {
			t.Fatalf("round trip mismatch: got %q want %q", dec, plain)
		}
	}
}

func TestEncryptEmptyIsEmpty(t *testing.T) {
	enc, err := Encrypt("secret", "")
	if err != nil {
		t.Fatalf("Encrypt empty error: %v", err)
	}
	if enc != "" {
		t.Fatalf("empty plaintext should encrypt to empty, got %q", enc)
	}
	dec, err := Decrypt("secret", "")
	if err != nil {
		t.Fatalf("Decrypt empty error: %v", err)
	}
	if dec != "" {
		t.Fatalf("empty ciphertext should decrypt to empty, got %q", dec)
	}
}

func TestEncryptUsesRandomNonce(t *testing.T) {
	const secret, plain = "secret", "sk-repeatable"
	a, err := Encrypt(secret, plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encrypt(secret, plain)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("expected distinct ciphertexts for repeated plaintext (random nonce), got identical")
	}
}

func TestDecryptWrongSecretFails(t *testing.T) {
	enc, err := Encrypt("right-secret", "sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt("wrong-secret", enc); err == nil {
		t.Fatalf("expected decrypt with wrong secret to fail, got nil error")
	}
}

func TestDecryptTamperedFails(t *testing.T) {
	enc, err := Encrypt("secret", "sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	// Flip a character in the base64 blob.
	tampered := []byte(enc)
	if tampered[len(tampered)-2] == 'A' {
		tampered[len(tampered)-2] = 'B'
	} else {
		tampered[len(tampered)-2] = 'A'
	}
	if _, err := Decrypt("secret", string(tampered)); err == nil {
		t.Fatalf("expected decrypt of tampered ciphertext to fail (GCM auth), got nil error")
	}
}

func TestMask(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"short":               "shor****hort", // len 5 -> head+tail=8 > len, but >? len 5 <=8 so "****"
		"sk-1234567890abcdef": "sk-1****cdef",
	}
	// Correct expectation for "short" (len 5 <= 8) is "****".
	cases["short"] = "****"
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}
