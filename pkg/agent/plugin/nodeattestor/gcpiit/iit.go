package gcpiit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/hashicorp/hcl"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/spiffe/spire/pkg/common/catalog"
	"github.com/spiffe/spire/pkg/common/plugin/gcp"
	nodeattestorv1 "github.com/vishnusomank/spire-plugin-sdk/proto/spire/plugin/agent/nodeattestor/v1"
	configv1 "github.com/vishnusomank/spire-plugin-sdk/proto/spire/service/common/config/v1"
)

const (
	defaultIdentityTokenHost     = "metadata.google.internal"
	identityTokenURLPathTemplate = "/computeMetadata/v1/instance/service-accounts/%s/identity"
	identityTokenAudience        = "spire-gcp-node-attestor" //nolint: gosec // false positive
	defaultServiceAccount        = "default"
)

func BuiltIn() catalog.BuiltIn {
	return builtin(New())
}

func builtin(p *IITAttestorPlugin) catalog.BuiltIn {
	return catalog.MakeBuiltIn(gcp.PluginName,
		nodeattestorv1.NodeAttestorPluginServer(p),
		configv1.ConfigServiceServer(p))
}

// IITAttestorPlugin implements GCP nodeattestation in the agent.
type IITAttestorPlugin struct {
	nodeattestorv1.UnsafeNodeAttestorServer
	configv1.UnsafeConfigServer

	mtx    sync.RWMutex
	config *IITAttestorConfig
}

// IITAttestorConfig configures a IITAttestorPlugin.
type IITAttestorConfig struct {
	IdentityTokenHost string `hcl:"identity_token_host"`
	ServiceAccount    string `hcl:"service_account"`
}

// NewIITAttestorPlugin creates a new IITAttestorPlugin.
func New() *IITAttestorPlugin {
	return &IITAttestorPlugin{}
}

// AidAttestation fetches attestation data from the GCP metadata server and sends an attestation response
// on given stream.
func (p *IITAttestorPlugin) AidAttestation(stream nodeattestorv1.NodeAttestor_AidAttestationServer) error {
	c, err := p.getConfig()
	if err != nil {
		return err
	}

	identityToken, err := retrieveInstanceIdentityToken(identityTokenURL(c.IdentityTokenHost, c.ServiceAccount))
	if err != nil {
		return status.Errorf(codes.Internal, "unable to retrieve valid identity token: %v", err)
	}

	return stream.Send(&nodeattestorv1.PayloadOrChallengeResponse{
		Data: &nodeattestorv1.PayloadOrChallengeResponse_Payload{
			Payload: identityToken,
		},
	})
}

func (p *IITAttestorPlugin) Configure(ctx context.Context, req *configv1.ConfigureRequest) (*configv1.ConfigureResponse, error) {
	config := &IITAttestorConfig{}
	if err := hcl.Decode(config, req.HclConfiguration); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "unable to decode configuration: %v", err)
	}

	if config.ServiceAccount == "" {
		config.ServiceAccount = defaultServiceAccount
	}

	if config.IdentityTokenHost == "" {
		config.IdentityTokenHost = defaultIdentityTokenHost
	}

	p.mtx.Lock()
	defer p.mtx.Unlock()
	p.config = config

	return &configv1.ConfigureResponse{}, nil
}

func (p *IITAttestorPlugin) getConfig() (*IITAttestorConfig, error) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.config == nil {
		return nil, status.Error(codes.FailedPrecondition, "not configured")
	}
	return p.config, nil
}

// identityTokenURL creates the URL to find an instance identity document given the
// host of the GCP metadata server and the service account the instance is running as.
func identityTokenURL(host, serviceAccount string) string {
	query := url.Values{}
	query.Set("audience", identityTokenAudience)
	query.Set("format", "full")
	url := &url.URL{
		Scheme:   "http",
		Host:     host,
		Path:     fmt.Sprintf(identityTokenURLPathTemplate, serviceAccount),
		RawQuery: query.Encode(),
	}
	return url.String()
}

func retrieveInstanceIdentityToken(url string) ([]byte, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return bytes, nil
}
