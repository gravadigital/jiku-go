package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
)

// generateTestKey builds a service account key file in memory, so the tests never carry a real
// credential and never need one on disk.
func generateTestKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	b, err := json.Marshal(ServiceAccountKey{
		Type: "serviceaccount", KeyID: "7", UserID: "42", Key: string(pemBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
