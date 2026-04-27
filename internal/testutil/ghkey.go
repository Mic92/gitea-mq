package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"sync"
)

// GithubAppKey returns a PEM-encoded 2048-bit RSA private key suitable for
// constructing a GitHub App transport in tests. ghinstallation refuses to
// initialise without valid PEM even though ghfake never verifies the JWT.
// Generated once per process because keygen dominates test runtime.
var GithubAppKey = sync.OnceValue(func() []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
})
