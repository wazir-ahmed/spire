package client

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/spiffe/spire/pkg/common/bundleutil"
	"github.com/spiffe/spire/test/fakes/fakedatastore"
	"github.com/spiffe/spire/test/spiretest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishnusomank/go-spiffe/v2/spiffeid"
)

func TestBundleUpdaterUpdateBundle(t *testing.T) {
	bundle1 := bundleutil.BundleFromRootCA(trustDomain, createCACertificate(t, "bundle1"))
	bundle2 := bundleutil.BundleFromRootCA(trustDomain, createCACertificate(t, "bundle2"))
	bundle2.SetRefreshHint(time.Minute)

	testCases := []struct {
		// name of the test
		name string
		// trust domain
		trustDomain spiffeid.TrustDomain
		// the bundle prepopulated in the datastore and returned from Update()
		localBundle *bundleutil.Bundle
		// the expected endpoint bundle returned from Update()
		endpointBundle *bundleutil.Bundle
		// the bundle in the datastore after Update()
		storedBundle *bundleutil.Bundle
		// the fake endpoint client
		client fakeClient
		// the expected error returned from Update()
		err string
	}{
		{
			name:        "providing no bundle",
			trustDomain: trustDomain,
			err:         "local copy of bundle not found",
		},
		{
			name:           "bundle has no changes",
			trustDomain:    trustDomain,
			localBundle:    bundle1,
			endpointBundle: nil,
			storedBundle:   bundle1,
			client: fakeClient{
				bundle: bundle1,
			},
		},
		{
			name:           "bundle changed",
			trustDomain:    trustDomain,
			localBundle:    bundle1,
			endpointBundle: bundle2,
			storedBundle:   bundle2,
			client: fakeClient{
				bundle: bundle2,
			},
		},
		{
			name:           "bundle fails to download",
			trustDomain:    trustDomain,
			localBundle:    bundle1,
			endpointBundle: nil,
			storedBundle:   bundle1,
			client: fakeClient{
				err: errors.New("ohno"),
			},
			err: "ohno",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			ds := fakedatastore.New(t)

			if testCase.localBundle != nil {
				_, err := ds.CreateBundle(context.Background(), testCase.localBundle.Proto())
				require.NoError(t, err)
			}

			updater := NewBundleUpdater(BundleUpdaterConfig{
				DataStore:   ds,
				TrustDomain: testCase.trustDomain,
				TrustDomainConfig: TrustDomainConfig{
					EndpointURL: "ENDPOINT_ADDRESS",
					EndpointProfile: HTTPSSPIFFEProfile{
						EndpointSPIFFEID: trustDomain.ID(),
					},
				},
				newClientHook: func(client ClientConfig) (Client, error) {
					return testCase.client, nil
				},
			})

			localBundle, endpointBundle, err := updater.UpdateBundle(context.Background())
			if testCase.err != "" {
				spiretest.RequireErrorContains(t, err, testCase.err)
				return
			}
			require.NoError(t, err)
			if testCase.localBundle != nil {
				require.NotNil(t, localBundle)
				spiretest.RequireProtoEqual(t, testCase.localBundle.Proto(), localBundle.Proto())
			} else {
				require.Nil(t, localBundle)
			}

			if testCase.endpointBundle != nil {
				require.NotNil(t, endpointBundle)
				spiretest.RequireProtoEqual(t, testCase.endpointBundle.Proto(), endpointBundle.Proto())
			} else {
				require.Nil(t, endpointBundle)
			}

			bundle, err := ds.FetchBundle(context.Background(), testCase.trustDomain.IDString())
			require.NoError(t, err)
			if testCase.storedBundle != nil {
				require.NotNil(t, bundle)
				spiretest.RequireProtoEqual(t, testCase.storedBundle.Proto(), bundle)
			} else {
				require.Nil(t, bundle)
			}
		})
	}
}

func TestBundleUpdaterConfiguration(t *testing.T) {
	configs := []TrustDomainConfig{
		{
			EndpointURL:     "https://some-domain.test/webA",
			EndpointProfile: HTTPSWebProfile{},
		},
		{
			EndpointURL:     "https://some-domain.test/webB",
			EndpointProfile: HTTPSWebProfile{},
		},
		{
			EndpointURL: "https://some-domain.test/spiffeA",
			EndpointProfile: HTTPSSPIFFEProfile{
				EndpointSPIFFEID: spiffeid.RequireFromString("spiffe://some-domain.test/spiffeA"),
			},
		},
		{
			EndpointURL: "https://some-domain.test/spiffeB",
			EndpointProfile: HTTPSSPIFFEProfile{
				EndpointSPIFFEID: spiffeid.RequireFromString("spiffe://some-domain.test/spiffeB"),
			},
		},
	}

	updater := NewBundleUpdater(BundleUpdaterConfig{})

	for _, config := range configs {
		assert.True(t, updater.SetTrustDomainConfig(config), "config should have changed")
		assert.False(t, updater.SetTrustDomainConfig(config), "config should not have changed")
	}
}

type fakeClient struct {
	bundle *bundleutil.Bundle
	err    error
}

func (c fakeClient) FetchBundle(context.Context) (*bundleutil.Bundle, error) {
	return c.bundle, c.err
}

func createCACertificate(t *testing.T, cn string) *x509.Certificate {
	now := time.Now()
	cert, _ := spiretest.SelfSignCertificate(t, &x509.Certificate{
		SerialNumber: big.NewInt(0),
		NotBefore:    now,
		NotAfter:     now.Add(time.Hour),
		IsCA:         true,
		Subject:      pkix.Name{CommonName: cn},
	})
	return cert
}
