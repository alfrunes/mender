package client

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io/ioutil"
	"net/http"

	"github.com/pkg/errors"
)

type AuthRequest struct {
	Identity    []byte `json:"id_data"`
	PublicKey   []byte `json:"pubkey"`
	TenantToken []byte `json:"tenant_token,omitempty"`
}

func (client *MenderClient) buildAuthRequest(server MenderServer) (*http.Request, error) {
	var err error
	var reqBody AuthRequest
	var privateDer []byte
	var publicDer []byte
	var signature []byte
	var privateKey interface{}
	privatePem, _ := pem.Decode(client.privateKey)
	if privatePem == nil {
		// Is key in DER form?
		privateDer = client.privateKey
	} else {
		privateDer, err = x509.DecryptPEMBlock(privatePem, nil)
		return nil, errors.Wrap(err, "error loading private key")
	}
	privateKey, err = x509.ParsePKCS8PrivateKey(privateDer)
	if err != nil {
		// Is key in PKCS1 format?
		privateKey, err = x509.ParsePKCS1PrivateKey(privateDer)
		if err != nil {
			// Is key elliptic?
			privateKey, err = x509.ParseECPrivateKey(privateDer)
			if err != nil {
				return nil, errors.Wrap(err,
					"malformed private key")
			}
		}
	}

	var privateSigner crypto.Signer
	switch privateKey.(type) {
	case rsa.PrivateKey,
		ecdsa.PrivateKey,
		ed25519.PrivateKey:

		// All the keys satisfy the crypto.Signer interface
		privateSigner := privateKey.(crypto.Signer)
		publicDer, err = x509.MarshalPKIXPublicKey(privateSigner.Public())
		if err != nil {
			return nil, errors.Wrap(err,
				"error serializing public privateSigner")
		}
	default:
		return nil, errors.New("private key not supported")
	}

	buf := &bytes.Buffer{}
	err = pem.Encode(buf, &pem.Block{
		Type:  "PUBLIC KEY", //PKIX
		Bytes: publicDer,
	})
	reqBody.PublicKey = buf.Bytes()
	reqBody.Identity = client.identity
	reqBody.TenantToken = server.TenantToken

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, errors.Wrap(err,
			"error serializing authorization request body")
	}

	// NOTE: ed25519 doesn't not support non-zero Hash argument, so we
	//       need to do this manually.
	hash := sha256.Sum256(body)
	// For RSA this will default to PKCS1 v1.5.
	signature, err = privateSigner.Sign(
		rand.Reader, hash[:], crypto.Hash(0))

	buf.Reset()
	_, err = buf.Write(body)
	if err != nil {
		return nil, errors.Wrap(err, "error preparing request body")
	}

	req, err := http.NewRequest(
		"GET",
		server.ServerURL+ApiAuthPath,
		buf,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MEN-Signature", string(signature))
	return req, nil
}

// Private function only makes a single attempt to authorize.
func (client *MenderClient) authorize(server *MenderServer) (*http.Response, error) {
	req, err := client.buildAuthRequest(*server)
	if err != nil {
		return nil, errors.Wrap(err, "error building request")
	}

	rsp, err := client.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if rsp.StatusCode == 200 {
		server.APIToken, err = ioutil.ReadAll(rsp.Body)
		err = errors.Wrap(err,
			"error extracting APIToken from HTTP-response")
	}
	return rsp, err

}

// Authorize issues an authentication request based on the data the client was
// initialized with.
func (client *MenderClient) Authorize() (*http.Response, error) {
	req, err := client.buildAuthRequest(client.servers[client.activeServer])
	if err != nil {
		return nil, errors.Wrap(err, "error building request")
	}

	return client.Do(req)
}
