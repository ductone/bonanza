package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	model_command "bonanza.build/pkg/model/command"
	"bonanza.build/pkg/proto/configuration/bonanza_worker"
	grpc_configuration "github.com/buildbarn/bb-storage/pkg/proto/configuration/grpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Native repository actions never share a process, platform, FUSE tree or
// runner with generic builds. These process-owned values cannot be supplied by
// the scheduler or by a Command. A signed operator attestation is additionally
// required before startup and before every action; these variables alone grant
// no capability.
func configuredRepositoryMode(configuration *bonanza_worker.ApplicationConfiguration) (*model_command.RepositoryMode, error) {
	keys := []string{
		"BONANZA_REPOSITORY_WORKER_ENVIRONMENT",
		"BONANZA_REPOSITORY_WORKER_REPOSITORY",
		"BONANZA_REPOSITORY_WORKER_ATTESTATION_PATH",
		"BONANZA_REPOSITORY_WORKER_AUTHORITY_PATH",
	}
	values := make([]string, len(keys))
	configured := false
	for i, key := range keys {
		values[i] = os.Getenv(key)
		configured = configured || values[i] != ""
	}
	if !configured {
		return nil, nil
	}
	if configuration == nil || len(configuration.ReapiRunners) != 0 || len(configuration.BuildDirectories) != 1 || len(configuration.BuildDirectories[0].Runners) != 1 {
		return nil, status.Error(codes.FailedPrecondition, "repository worker requires one dedicated native runner and no REAPI runners")
	}
	for _, service := range []*grpc_configuration.ClientConfiguration{configuration.StorageGrpcClient, configuration.SchedulerGrpcClient} {
		tls := service.GetTls()
		files := tls.GetClientKeyPair().GetFiles()
		if service.GetAddress() == "" || strings.HasPrefix(service.GetAddress(), "unix:") ||
			service.GetProxyUrl() != "" || len(service.GetAddMetadata()) != 0 || service.GetOauth2() != nil ||
			tls.GetServerCertificateAuthorities() == "" || files == nil ||
			!filepath.IsAbs(files.GetCertificatePath()) || !filepath.IsAbs(files.GetPrivateKeyPath()) {
			return nil, status.Error(codes.FailedPrecondition, "repository worker requires mTLS to Bonanza services without forwarded credentials")
		}
	}
	build := configuration.BuildDirectories[0]
	runner := build.Runners[0]
	if runner.ClientCertificateVerifier.GetClientCertificateAuthorities() == "" ||
		runner.ClientCertificateVerifier.GetMetadataExtractionJmespathExpression().GetExpression() == "" ||
		runner.ClientCertificateVerifier.GetMetadataExtractionJmespathExpression().GetExpression() == "`{}`" ||
		len(runner.PlatformPrivateKeys) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "repository worker requires authenticated client claims and a dedicated platform key")
	}
	if build.Mount.GetFuse() == nil || runner.Concurrency != 1 || len(runner.EnvironmentVariables) != 0 ||
		runner.BuildDirectoryOwnerUserId == 0 || runner.MaximumFilePoolSizeBytes == 0 ||
		runner.MaximumFilePoolSizeBytes > 1<<30 || runner.MaximumFilePoolFileCount == 0 ||
		runner.MaximumFilePoolFileCount > 100000 || configuration.FilePool == nil ||
		runner.MaximumExecutionTimeoutCompensation == nil || runner.MaximumWritableFileUploadDelay == nil ||
		runner.MaximumExecutionTimeoutCompensation.AsDuration() < 0 ||
		runner.MaximumExecutionTimeoutCompensation.AsDuration() > time.Minute ||
		runner.MaximumWritableFileUploadDelay.AsDuration() < 0 ||
		runner.MaximumWritableFileUploadDelay.AsDuration() > time.Minute {
		return nil, status.Error(codes.FailedPrecondition, "repository worker requires FUSE, one uncredentialed runner and bounded file/time resources")
	}
	address := runner.Endpoint.GetAddress()
	if !strings.HasPrefix(address, "unix:///") {
		return nil, status.Error(codes.FailedPrecondition, "repository runner must use a local Unix socket")
	}
	publicKey, err := os.ReadFile(values[3])
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "repository isolation signing authority unavailable")
	}
	return model_command.NewRepositoryMode(values[0], values[1], build.Mount.GetMountPath(), strings.TrimPrefix(address, "unix://"), values[2], string(publicKey), runner.BuildDirectoryOwnerUserId)
}
