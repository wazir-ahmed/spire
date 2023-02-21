package x509pop

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint: gosec // SHA1 use is according to specification
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/spiffe/spire/pkg/common/agentpathtemplate"
	"github.com/spiffe/spire/pkg/common/idutil"
	"github.com/vishnusomank/go-spiffe/v2/spiffeid"
)

const (
	nonceLen = 32

	// PluginName for X.509 Proof of Possession
	PluginName = "x509pop"
)

// DefaultAgentPathTemplate is the default template
var DefaultAgentPathTemplate = agentpathtemplate.MustParse("/{{ .PluginName }}/{{ .Fingerprint }}")

type agentPathTemplateData struct {
	*x509.Certificate
	Fingerprint string
	PluginName  string
	TrustDomain string
}

type AttestationData struct {
	// DER encoded x509 certificate chain leading back to the trusted root. The
	// leaf certificate comes first.
	Certificates [][]byte `json:"certificates"`
}

type RSASignatureChallenge struct {
	// Nonce is the nonce generated by the challenger.
	Nonce []byte `json:"nonce"`
}

type RSASignatureResponse struct {
	// Nonce is the nonce generated by the responder.
	Nonce []byte `json:"nonce"`

	// Signature is the RSA signature of the combined challenger and responder
	// nonces.
	Signature []byte `json:"signature"`
}

type ECDSASignatureChallenge struct {
	// Nonce is the nonce generated by the challenger.
	Nonce []byte `json:"nonce"`
}

type ECDSASignatureResponse struct {
	// Nonce is the nonce generated by the responder.
	Nonce []byte `json:"nonce"`

	// R value of the ECDSA signature of the combined challenger and responder
	// nonces.
	R []byte `json:"r"`

	// S value of the ECDSA signature of the combined challenger and responder
	// nonces.
	S []byte `json:"s"`
}

type Challenge struct {
	RSASignature   *RSASignatureChallenge   `json:"rsa_signature"`
	ECDSASignature *ECDSASignatureChallenge `json:"ecdsa_signature"`
}

type Response struct {
	RSASignature   *RSASignatureResponse   `json:"rsa_signature"`
	ECDSASignature *ECDSASignatureResponse `json:"ecdsa_signature"`
}

func GenerateChallenge(cert *x509.Certificate) (*Challenge, error) {
	// ensure that the public key is intended to be used for digital signatures
	if (cert.KeyUsage & x509.KeyUsageDigitalSignature) == 0 {
		return nil, errors.New("certificate not intended for digital signature use")
	}

	switch publicKey := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		challenge, err := GenerateRSASignatureChallenge()
		if err != nil {
			return nil, err
		}
		return &Challenge{
			RSASignature: challenge,
		}, nil
	case *ecdsa.PublicKey:
		challenge, err := GenerateECDSASignatureChallenge()
		if err != nil {
			return nil, err
		}
		return &Challenge{
			ECDSASignature: challenge,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported public key type %T", publicKey)
	}
}

func CalculateResponse(privateKey interface{}, challenge *Challenge) (*Response, error) {
	switch privateKey := privateKey.(type) {
	case *rsa.PrivateKey:
		rsaChallenge := challenge.RSASignature
		if rsaChallenge == nil {
			return nil, errors.New("expecting RSA challenge")
		}
		response, err := CalculateRSASignatureResponse(privateKey, rsaChallenge)
		if err != nil {
			return nil, err
		}
		return &Response{
			RSASignature: response,
		}, nil
	case *ecdsa.PrivateKey:
		if challenge.ECDSASignature == nil {
			return nil, errors.New("expecting ECDSA challenge")
		}
		response, err := CalculateECDSASignatureResponse(privateKey, challenge.ECDSASignature)
		if err != nil {
			return nil, err
		}
		return &Response{
			ECDSASignature: response,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported private key type %T", privateKey)
	}
}

func VerifyChallengeResponse(publicKey interface{}, challenge *Challenge, response *Response) error {
	switch publicKey := publicKey.(type) {
	case *rsa.PublicKey:
		if challenge.RSASignature == nil {
			return errors.New("expecting RSA challenge")
		}
		if response.RSASignature == nil {
			return errors.New("expecting RSA response")
		}
		return VerifyRSASignatureResponse(publicKey, challenge.RSASignature, response.RSASignature)
	case *ecdsa.PublicKey:
		if challenge.ECDSASignature == nil {
			return errors.New("expecting ECDSA challenge")
		}
		if response.ECDSASignature == nil {
			return errors.New("expecting ECDSA response")
		}
		return VerifyECDSASignatureResponse(publicKey, challenge.ECDSASignature, response.ECDSASignature)
	default:
		return fmt.Errorf("unsupported private key type %T", publicKey)
	}
}

func GenerateRSASignatureChallenge() (*RSASignatureChallenge, error) {
	nonce, err := generateNonce()
	if err != nil {
		return nil, err
	}

	return &RSASignatureChallenge{
		Nonce: nonce,
	}, nil
}

func CalculateRSASignatureResponse(privateKey *rsa.PrivateKey, challenge *RSASignatureChallenge) (*RSASignatureResponse, error) {
	nonce, err := generateNonce()
	if err != nil {
		return nil, err
	}

	combined, err := combineNonces(challenge.Nonce, nonce)
	if err != nil {
		return nil, err
	}

	signature, err := rsa.SignPSS(rand.Reader, privateKey, crypto.SHA256, combined, nil)
	if err != nil {
		return nil, err
	}

	return &RSASignatureResponse{
		Signature: signature,
		Nonce:     nonce,
	}, nil
}

func VerifyRSASignatureResponse(publicKey *rsa.PublicKey, challenge *RSASignatureChallenge, response *RSASignatureResponse) error {
	combined, err := combineNonces(challenge.Nonce, response.Nonce)
	if err != nil {
		return err
	}

	if err := rsa.VerifyPSS(publicKey, crypto.SHA256, combined, response.Signature, nil); err != nil {
		return errors.New("RSA signature verify failed")
	}
	return nil
}

func GenerateECDSASignatureChallenge() (*ECDSASignatureChallenge, error) {
	nonce, err := generateNonce()
	if err != nil {
		return nil, err
	}

	return &ECDSASignatureChallenge{
		Nonce: nonce,
	}, nil
}

func CalculateECDSASignatureResponse(privateKey *ecdsa.PrivateKey, challenge *ECDSASignatureChallenge) (*ECDSASignatureResponse, error) {
	nonce, err := generateNonce()
	if err != nil {
		return nil, err
	}

	combined, err := combineNonces(challenge.Nonce, nonce)
	if err != nil {
		return nil, err
	}

	r, s, err := ecdsa.Sign(rand.Reader, privateKey, combined)
	if err != nil {
		return nil, err
	}

	return &ECDSASignatureResponse{
		Nonce: nonce,
		R:     r.Bytes(),
		S:     s.Bytes(),
	}, nil
}

func VerifyECDSASignatureResponse(publicKey *ecdsa.PublicKey, challenge *ECDSASignatureChallenge, response *ECDSASignatureResponse) error {
	combined, err := combineNonces(challenge.Nonce, response.Nonce)
	if err != nil {
		return err
	}

	r := new(big.Int)
	r.SetBytes(response.R)
	s := new(big.Int)
	s.SetBytes(response.S)
	if !ecdsa.Verify(publicKey, combined, r, s) {
		return errors.New("ECDSA signature verify failed")
	}
	return nil
}

func Fingerprint(cert *x509.Certificate) string {
	sum := sha1.Sum(cert.Raw) //nolint: gosec // SHA1 use is according to specification
	return hex.EncodeToString(sum[:])
}

// MakeAgentID creates an agent ID from X.509 certificate data.
func MakeAgentID(td spiffeid.TrustDomain, agentPathTemplate *agentpathtemplate.Template, cert *x509.Certificate) (spiffeid.ID, error) {
	agentPath, err := agentPathTemplate.Execute(agentPathTemplateData{
		Certificate: cert,
		PluginName:  PluginName,
		Fingerprint: Fingerprint(cert),
	})
	if err != nil {
		return spiffeid.ID{}, err
	}

	return idutil.AgentID(td, agentPath)
}

func generateNonce() ([]byte, error) {
	b := make([]byte, nonceLen)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func combineNonces(challenge, response []byte) ([]byte, error) {
	if len(challenge) != nonceLen {
		return nil, errors.New("invalid challenge nonce")
	}
	if len(response) != nonceLen {
		return nil, errors.New("invalid response nonce")
	}
	h := sha256.New()
	// write the challenge and response and ignore errors since it won't fail
	// writing to the digest
	_, _ = h.Write(challenge)
	_, _ = h.Write(response)
	return h.Sum(nil), nil
}
