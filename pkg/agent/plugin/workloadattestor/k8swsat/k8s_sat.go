package k8swsat

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/andres-erbsen/clock"
	"github.com/golang-jwt/jwt"
	"github.com/hashicorp/go-hclog"
	"github.com/spiffe/spire/pkg/common/catalog"
	workloadattestorv1 "github.com/vishnusomank/spire-plugin-sdk/proto/spire/plugin/agent/workloadattestor/v1"
	configv1 "github.com/vishnusomank/spire-plugin-sdk/proto/spire/service/common/config/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	pluginName               = "k8sw_sat"
	defaultMaxPollAttempts   = 60
	defaultPollRetryInterval = time.Millisecond * 500
	defaultReloadInterval    = time.Minute
)

func BuiltIn() catalog.BuiltIn {
	return builtin(New())
}

func builtin(p *Plugin) catalog.BuiltIn {
	return catalog.MakeBuiltIn(pluginName,
		workloadattestorv1.WorkloadAttestorPluginServer(p),
	)
}

type Plugin struct {
	workloadattestorv1.UnsafeWorkloadAttestorServer
	configv1.UnsafeConfigServer

	log   hclog.Logger
	clock clock.Clock
	mtx   sync.RWMutex
}

func New() *Plugin {
	return &Plugin{
		clock: clock.New(),
	}
}

func (p *Plugin) SetLogger(log hclog.Logger) {
	p.log = log
}

func (p *Plugin) Attest(ctx context.Context, req *workloadattestorv1.AttestRequest) (*workloadattestorv1.AttestResponse, error) {

	var log hclog.Logger
	p.mtx.RLock()
	defer p.mtx.RUnlock()

	for attempt := 1; ; attempt++ {

		var selectorValues []string

		var attestResponse *workloadattestorv1.AttestResponse
		token := defaultSAToken
		selectorValues = append(selectorValues, getSelectorValuesFromToken(token)...)

		if len(selectorValues) > 0 {
			attestResponse = &workloadattestorv1.AttestResponse{SelectorValues: selectorValues}
		}

		if attestResponse != nil {
			return attestResponse, nil
		}

		// if the container was not located after the maximum number of attempts then the search is over.
		if attempt >= defaultMaxPollAttempts {
			log.Warn("Decoding failed; giving up")
			return nil, status.Error(codes.DeadlineExceeded, "no selectors found after max poll attempts")
		}

		select {
		case <-p.clock.After(defaultPollRetryInterval):
		case <-ctx.Done():
			return nil, status.Errorf(codes.Canceled, "no selectors found: %v", ctx.Err())
		}
	}

}

func (p *Plugin) Configure(ctx context.Context, req *configv1.ConfigureRequest) (resp *configv1.ConfigureResponse, err error) {
	return &configv1.ConfigureResponse{}, nil
}

func getSelectorValuesFromToken(token string) []string {

	tokenMap, _, err := new(jwt.Parser).ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		fmt.Printf("Error parsing token: %v", err)
		return []string{}
	}
	saAndNs := tokenMap.Claims.(jwt.MapClaims)["kubernetes.io"].(map[string]interface{})
	namespace := saAndNs["namespace"]
	saName := saAndNs["serviceaccount"].(map[string]interface{})["name"]

	selectorValues := []string{
		fmt.Sprintf("sa:%s", saName),
		fmt.Sprintf("ns:%s", namespace),
	}

	return selectorValues
}

/*
func main() {
	plugin := new(Plugin)
	// Serve the plugin. This function call will not return. If there is a
	// failure to serve, the process will exit with a non-zero exit code.
	pluginmain.Serve(
		workloadattestorv1.WorkloadAttestorPluginServer(plugin),
		// TODO: Remove if no configuration is required
		configv1.ConfigServiceServer(plugin),
	)
}
*/
