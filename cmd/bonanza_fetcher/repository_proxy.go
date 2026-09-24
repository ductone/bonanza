package main

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"time"

	model_fetch "bonanza.build/pkg/model/fetch"

	http_client "github.com/buildbarn/bb-storage/pkg/http/client"
	http_client_pb "github.com/buildbarn/bb-storage/pkg/proto/configuration/http/client"
	"github.com/buildbarn/bb-storage/pkg/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// validateRepositoryProxyConfiguration prevents the fetcher's mTLS identity
// from being sent to a direct origin or an arbitrary HTTP CONNECT proxy. A
// configured repository fetcher never falls back to the generic HTTP client.
func validateRepositoryProxyConfiguration(configuration *http_client_pb.Configuration, endpoint, environment, repository string) error {
	configured := endpoint != "" || environment != "" || repository != ""
	if !configured {
		if configuration.GetTls().GetClientKeyPair() != nil {
			return status.Error(codes.FailedPrecondition, "fetcher client certificate requires repository proxy configuration")
		}
		return nil
	}
	// The environment value is only a local comparison constraint. The proxy
	// independently resolves the authenticated worker certificate to its
	// registered environment and repository.
	for _, c := range environment {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return status.Error(codes.FailedPrecondition, "invalid repository worker environment identity")
		}
	}
	if endpoint == "" || environment == "" || repository == "" || configuration == nil {
		return status.Error(codes.FailedPrecondition, "repository proxy requires endpoint, environment, repository, and HTTP client")
	}
	if configuration.GetProxyUrl() != "" || len(configuration.GetAddHeaders()) != 0 || configuration.GetOauth2() != nil {
		return status.Error(codes.FailedPrecondition, "repository proxy cannot use an upstream proxy or credentialed headers")
	}
	keyPair := configuration.GetTls().GetClientKeyPair()
	if keyPair == nil {
		return status.Error(codes.FailedPrecondition, "repository proxy requires a worker mTLS certificate")
	}
	var certificate []byte
	if inline := keyPair.GetInline(); inline != nil {
		certificate = []byte(inline.GetCertificate())
	} else if files := keyPair.GetFiles(); files != nil {
		var err error
		certificate, err = os.ReadFile(files.GetCertificatePath())
		if err != nil {
			return status.Error(codes.FailedPrecondition, "repository proxy worker certificate unavailable")
		}
	}
	block, _ := pem.Decode(certificate)
	if block == nil || block.Type != "CERTIFICATE" {
		return status.Error(codes.FailedPrecondition, "repository proxy worker certificate missing")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil || len(leaf.URIs) != 1 ||
		leaf.URIs[0].String() != "spiffe://squire.ductone.com/bonanza/repository-worker/"+environment ||
		time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return status.Error(codes.FailedPrecondition, "repository proxy worker certificate identity is missing, mismatched, or expired")
	}
	return nil
}

// newHTTPTransport loads the worker's mTLS identity only after configuration
// validation rules out direct and credentialed fallback.
func newHTTPTransport(configuration *http_client_pb.Configuration) (*http.Client, error) {
	roundTripper, err := http_client.NewRoundTripperFromConfiguration(configuration)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to create HTTP client")
	}
	return &http.Client{Transport: roundTripper}, nil
}

// Proxy configuration is process-owned; no action can set these values. The
// gateway-issued worker certificate and registration remain the proxy's source
// of identity, and the verified action certificate is checked by the fetcher.
func configuredRepositoryFetcher(configuration *http_client_pb.Configuration, endpoint, environment, repository string) (model_fetch.Fetcher, error) {
	if err := validateRepositoryProxyConfiguration(configuration, endpoint, environment, repository); err != nil {
		return nil, err
	}
	if configuration == nil {
		return nil, nil
	}
	transport, err := newHTTPTransport(configuration)
	if err != nil {
		return nil, err
	}
	if endpoint == "" {
		return model_fetch.NewHTTPFetcher(transport), nil
	}
	return model_fetch.NewRepositoryProxyFetcher(transport, endpoint, environment, repository)
}
