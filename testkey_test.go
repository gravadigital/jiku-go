package jiku

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
)

// testServiceAccountKey builds a Zitadel-shaped service account key in memory, so the tests never
// carry a real credential and never need one on disk.
func testServiceAccountKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	b, err := json.Marshal(map[string]string{
		"type": "serviceaccount", "keyId": "7", "userId": "42", "key": string(pemBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
