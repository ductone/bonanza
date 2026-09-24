package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bonanza.build/pkg/proto/configuration/bonanza_worker"
	filesystem_configuration "github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/filesystem"
	virtual_configuration "github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/filesystem/virtual"
	grpc_configuration "github.com/buildbarn/bb-storage/pkg/proto/configuration/grpc"
	jmespath_configuration "github.com/buildbarn/bb-storage/pkg/proto/configuration/jmespath"
	tls_configuration "github.com/buildbarn/bb-storage/pkg/proto/configuration/tls"
	x509_configuration "github.com/buildbarn/bb-storage/pkg/proto/configuration/x509"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestRepositoryModeRequiresDedicatedNativeProcess(t *testing.T) {
	for _, key := range []string{
		"BONANZA_REPOSITORY_WORKER_ENVIRONMENT",
		"BONANZA_REPOSITORY_WORKER_REPOSITORY",
		"BONANZA_REPOSITORY_WORKER_ATTESTATION_PATH",
		"BONANZA_REPOSITORY_WORKER_AUTHORITY_PATH",
	} {
		t.Setenv(key, "")
	}
	if mode, err := configuredRepositoryMode(&bonanza_worker.ApplicationConfiguration{}); err != nil || mode != nil {
		t.Fatalf("default generic worker = %v, %v", mode, err)
	}
	t.Setenv("BONANZA_REPOSITORY_WORKER_ENVIRONMENT", "env-a")
	for name, config := range map[string]*bonanza_worker.ApplicationConfiguration{
		"no native runner":      {},
		"REAPI runner":          {ReapiRunners: []*bonanza_worker.ReapiRunnerConfiguration{{}}},
		"shared native runners": {BuildDirectories: []*bonanza_worker.BuildDirectoryConfiguration{{Runners: []*bonanza_worker.RunnerConfiguration{{}, {}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := configuredRepositoryMode(config); status.Code(err) != codes.FailedPrecondition ||
				!strings.Contains(err.Error(), "one dedicated native runner") {
				t.Fatalf("shared repository worker configuration accepted: %v", err)
			}
		})
	}
	// Reach the FUSE guard with all earlier identity and transport
	// prerequisites satisfied: otherwise a missing-service error would
	// make this test insensitive to an accidentally disabled FUSE check.
	service := &grpc_configuration.ClientConfiguration{
		Address: "core.example:443",
		Tls: &tls_configuration.ClientConfiguration{
			ServerCertificateAuthorities: "operator CA",
			ClientKeyPair: &tls_configuration.X509KeyPair{KeyPair: &tls_configuration.X509KeyPair_Files_{
				Files: &tls_configuration.X509KeyPair_Files{CertificatePath: "/tmp/operator.crt", PrivateKeyPath: "/tmp/operator.key"},
			}},
		},
	}
	runner := &bonanza_worker.RunnerConfiguration{
		Concurrency: 1, PlatformPrivateKeys: []string{"operator key"},
		ClientCertificateVerifier: &x509_configuration.ClientCertificateVerifierConfiguration{
			ClientCertificateAuthorities:         "operator CA",
			MetadataExtractionJmespathExpression: &jmespath_configuration.Expression{Expression: "public"},
		},
		BuildDirectoryOwnerUserId: 1234, MaximumFilePoolSizeBytes: 1024, MaximumFilePoolFileCount: 10,
		MaximumExecutionTimeoutCompensation: durationpb.New(time.Second),
		MaximumWritableFileUploadDelay:      durationpb.New(time.Second),
	}
	config := &bonanza_worker.ApplicationConfiguration{
		StorageGrpcClient: service, SchedulerGrpcClient: service,
		FilePool:         &filesystem_configuration.FilePoolConfiguration{},
		BuildDirectories: []*bonanza_worker.BuildDirectoryConfiguration{{Runners: []*bonanza_worker.RunnerConfiguration{runner}}},
	}
	if _, err := configuredRepositoryMode(config); status.Code(err) != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), "requires FUSE") {
		t.Fatalf("repository worker without FUSE accepted: %v", err)
	}
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	authorityPath := filepath.Join(t.TempDir(), "authority.pem")
	if err := os.WriteFile(authorityPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BONANZA_REPOSITORY_WORKER_REPOSITORY", "ductone/c1")
	t.Setenv("BONANZA_REPOSITORY_WORKER_AUTHORITY_PATH", authorityPath)
	t.Setenv("BONANZA_REPOSITORY_WORKER_ATTESTATION_PATH", filepath.Join(t.TempDir(), "not-issued"))
	runner.Endpoint = &grpc_configuration.ClientConfiguration{Address: "unix:///tmp/repository-runner.sock"}
	config.BuildDirectories[0].Mount = &virtual_configuration.MountConfiguration{
		MountPath: "/tmp/repository-mount",
		Backend:   &virtual_configuration.MountConfiguration_Fuse{Fuse: &virtual_configuration.FUSEMountConfiguration{}},
	}
	mode, err := configuredRepositoryMode(config)
	if err != nil || mode == nil {
		t.Fatalf("dedicated mode configuration did not bind process: %v", err)
	}
	if err := mode.Verify(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unissued isolation authority unexpectedly enabled lane: %v", err)
	}
}
