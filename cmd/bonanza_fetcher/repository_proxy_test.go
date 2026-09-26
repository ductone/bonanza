package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	http_client_pb "github.com/buildbarn/bb-storage/pkg/proto/configuration/http/client"
	tls_pb "github.com/buildbarn/bb-storage/pkg/proto/configuration/tls"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func testWorkerKeyPair(t *testing.T, environment string) (string, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test worker"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if environment != "" {
		template.URIs = []*url.URL{{Scheme: "spiffe", Host: "squire.ductone.com", Path: "/bonanza/repository-worker/" + environment}}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}))
}

func testWorkerCert(t *testing.T, environment string) string {
	certificate, _ := testWorkerKeyPair(t, environment)
	return certificate
}

func proxyHTTPConfig(certificate string) *http_client_pb.Configuration {
	return &http_client_pb.Configuration{Tls: &tls_pb.ClientConfiguration{
		ClientKeyPair: &tls_pb.X509KeyPair{KeyPair: &tls_pb.X509KeyPair_Inline_{Inline: &tls_pb.X509KeyPair_Inline{Certificate: certificate}}},
	}}
}

func TestRepositoryFetcherStartupRequiresIdentityAndNoCredentialFallback(t *testing.T) {
	const endpoint = "https://proxy.example/v1/repository-fetch"
	for name, testcase := range map[string]struct {
		config                      *http_client_pb.Configuration
		endpoint, environment, repo string
	}{
		"missing HTTP client":     {endpoint: endpoint, environment: "env-a", repo: "ductone/c1"},
		"partial environment":     {config: &http_client_pb.Configuration{}, endpoint: endpoint, repo: "ductone/c1"},
		"missing worker cert":     {config: &http_client_pb.Configuration{}, endpoint: endpoint, environment: "env-a", repo: "ductone/c1"},
		"empty URI SAN":           {config: proxyHTTPConfig(testWorkerCert(t, "")), endpoint: endpoint, environment: "env-a", repo: "ductone/c1"},
		"mismatched URI SAN":      {config: proxyHTTPConfig(testWorkerCert(t, "env-b")), endpoint: endpoint, environment: "env-a", repo: "ductone/c1"},
		"cert with direct origin": {config: proxyHTTPConfig(testWorkerCert(t, "env-a"))},
		"upstream CONNECT proxy": {config: func() *http_client_pb.Configuration {
			c := proxyHTTPConfig(testWorkerCert(t, "env-a"))
			c.ProxyUrl = "http://evil.example"
			return c
		}(), endpoint: endpoint, environment: "env-a", repo: "ductone/c1"},
		"credentialed HTTP header": {config: func() *http_client_pb.Configuration {
			c := proxyHTTPConfig(testWorkerCert(t, "env-a"))
			c.AddHeaders = []*http_client_pb.Configuration_HeaderValues{{Header: "Authorization", Values: []string{"token"}}}
			return c
		}(), endpoint: endpoint, environment: "env-a", repo: "ductone/c1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := configuredRepositoryFetcher(testcase.config, testcase.endpoint, testcase.environment, testcase.repo); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("configuration unexpectedly accepted: %v", err)
			}
		})
	}
	if fetcher, err := configuredRepositoryFetcher(nil, "", "", ""); err != nil || fetcher != nil {
		t.Fatalf("unconfigured fetcher = %v, %v", fetcher, err)
	}
	certificate, privateKey := testWorkerKeyPair(t, "env-a")
	validConfig := proxyHTTPConfig(certificate)
	validConfig.Tls.ClientKeyPair.GetInline().PrivateKey = privateKey
	if fetcher, err := configuredRepositoryFetcher(validConfig, endpoint, "env-a", "ductone/c1"); err != nil || fetcher == nil {
		t.Fatalf("valid proxy-bound fetcher = %v, %v", fetcher, err)
	}
}
