// Package sshpop implements ssh proof of possession based node attestation.
package sshpop

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/hcl"
	"github.com/spiffe/spire/pkg/common/agentpathtemplate"
	"github.com/vishnusomank/go-spiffe/v2/spiffeid"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// PluginName is used for identifying this plugin type for protobuf blobs.
	PluginName = "sshpop"

	defaultHostKeyPath  = "/etc/ssh/ssh_host_rsa_key"
	defaultHostCertPath = "/etc/ssh/ssh_host_rsa_key-cert.pub"
	nonceLen            = 32
)

var (
	// DefaultAgentPathTemplate is the default text/template.
	DefaultAgentPathTemplate = agentpathtemplate.MustParse("/{{ .PluginName}}/{{ .Fingerprint }}")
)

// agentPathTemplateData is used to hydrate the agent path template used in generating spiffe ids.
type agentPathTemplateData struct {
	*ssh.Certificate
	PluginName  string
	Fingerprint string
	Hostname    string
}

// Client is a factory for generating client handshake objects.
type Client struct {
	cert   *ssh.Certificate
	signer ssh.Signer
}

// Server is a factory for generating server handshake objects.
type Server struct {
	certChecker       *ssh.CertChecker
	agentPathTemplate *agentpathtemplate.Template
	trustDomain       spiffeid.TrustDomain
	canonicalDomain   string
}

// ClientConfig configures the client.
type ClientConfig struct {
	HostKeyPath  string `hcl:"host_key_path"`
	HostCertPath string `hcl:"host_cert_path"`
}

// ServerConfig configures the server.
type ServerConfig struct {
	CertAuthorities     []string `hcl:"cert_authorities"`
	CertAuthoritiesPath string   `hcl:"cert_authorities_path"`
	// CanonicalDomain specifies the domain suffix for validating the hostname against
	// the certificate's valid principals. See CanonicalDomains in ssh_config(5).
	CanonicalDomain   string `hcl:"canonical_domain"`
	AgentPathTemplate string `hcl:"agent_path_template"`
}

func NewClient(configString string) (*Client, error) {
	config := new(ClientConfig)
	if err := hcl.Decode(config, configString); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to decode configuration: %v", err)
	}
	config.HostKeyPath = stringOrDefault(config.HostKeyPath, defaultHostKeyPath)
	config.HostCertPath = stringOrDefault(config.HostCertPath, defaultHostCertPath)
	keyBytes, err := os.ReadFile(config.HostKeyPath)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to read host key file: %v", err)
	}
	certBytes, err := os.ReadFile(config.HostCertPath)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to read host cert file: %v", err)
	}
	cert, signer, err := getCertAndSignerFromBytes(certBytes, keyBytes)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to get cert and signer from pem: %v", err)
	}
	return &Client{
		cert:   cert,
		signer: signer,
	}, nil
}

func stringOrDefault(configValue, defaultValue string) string {
	if configValue == "" {
		return defaultValue
	}
	return configValue
}

func getCertAndSignerFromBytes(certBytes, keyBytes []byte) (*ssh.Certificate, ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, nil, err
	}
	pubkey, _, _, _, err := ssh.ParseAuthorizedKey(certBytes)
	if err != nil {
		return nil, nil, err
	}
	cert, ok := pubkey.(*ssh.Certificate)
	if !ok {
		return nil, nil, errors.New("pubkey isn't a certificate")
	}
	return cert, signer, nil
}

func NewServer(trustDomain, configString string) (*Server, error) {
	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "trust_domain global configuration is invalid: %v", err)
	}
	config := new(ServerConfig)
	if err := hcl.Decode(config, configString); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to decode configuration: %v", err)
	}
	if config.CertAuthorities == nil && config.CertAuthoritiesPath == "" {
		return nil, status.Errorf(codes.InvalidArgument, "missing required config value for \"cert_authorities\" or \"cert_authorities_path\"")
	}
	var certAuthorities []string
	if config.CertAuthorities != nil {
		certAuthorities = append(certAuthorities, config.CertAuthorities...)
	}
	if config.CertAuthoritiesPath != "" {
		fileCertAuthorities, err := pubkeysFromPath(config.CertAuthoritiesPath)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to get cert authorities from file: %v", err)
		}
		certAuthorities = append(certAuthorities, fileCertAuthorities...)
	}
	certChecker, err := certCheckerFromPubkeys(certAuthorities)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create cert checker: %v", err)
	}
	agentPathTemplate := DefaultAgentPathTemplate
	if len(config.AgentPathTemplate) > 0 {
		tmpl, err := agentpathtemplate.Parse(config.AgentPathTemplate)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to parse agent svid template: %q", config.AgentPathTemplate)
		}
		agentPathTemplate = tmpl
	}
	return &Server{
		certChecker:       certChecker,
		agentPathTemplate: agentPathTemplate,
		trustDomain:       td,
		canonicalDomain:   config.CanonicalDomain,
	}, nil
}

func pubkeysFromPath(pubkeysPath string) ([]string, error) {
	pubkeysBytes, err := os.ReadFile(pubkeysPath)
	if err != nil {
		return nil, err
	}
	splitPubkeys := strings.Split(string(pubkeysBytes), "\n")
	var pubkeys []string
	for _, pubkey := range splitPubkeys {
		if pubkey == "" {
			continue
		}
		pubkeys = append(pubkeys, pubkey)
	}
	if pubkeys == nil {
		return nil, fmt.Errorf("no data found in file: %q", pubkeysPath)
	}
	return pubkeys, nil
}

func certCheckerFromPubkeys(certAuthorities []string) (*ssh.CertChecker, error) {
	if len(certAuthorities) == 0 {
		return nil, errors.New("must provide at least one cert authority")
	}
	authorities := make(map[string]bool)
	for _, certAuthority := range certAuthorities {
		authority, _, _, _, err := ssh.ParseAuthorizedKey([]byte(certAuthority))
		if err != nil {
			return nil, fmt.Errorf("failed to parse public key %q: %w", certAuthority, err)
		}
		authorities[ssh.FingerprintSHA256(authority)] = true
	}
	return &ssh.CertChecker{
		IsHostAuthority: func(auth ssh.PublicKey, _ string) bool {
			return authorities[ssh.FingerprintSHA256(auth)]
		},
	}, nil
}

func (c *Client) NewHandshake() *ClientHandshake {
	return &ClientHandshake{
		c: c,
	}
}

func (s *Server) NewHandshake() *ServerHandshake {
	return &ServerHandshake{
		s: s,
	}
}
