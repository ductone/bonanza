package command

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	model_command_pb "bonanza.build/pkg/proto/model/command"
	"github.com/buildbarn/bb-storage/pkg/auth"
	"github.com/google/uuid"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNativeRunnerRejectsStatefulRepositoryActions(t *testing.T) {
	for name, command := range map[string]*model_command_pb.Command{
		"writable input tree": {NeedsWritableInputFiles: true},
		"stable input root":   {StableInputRootPathUuid: "123e4567-e89b-12d3-a456-426614174000"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateNativeCommand(command); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("native command unexpectedly accepted: %v", err)
			}
		})
	}
	if err := validateNativeCommand(&model_command_pb.Command{}); err != nil {
		t.Fatalf("stateless build action rejected: %v", err)
	}
}

func repositoryModeFixture(t *testing.T) (*RepositoryMode, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	mode, err := NewRepositoryMode("env-a", "ductone/c1", "/tmp/repository-mount", "/tmp/repository-runner.sock", filepath.Join(t.TempDir(), "attestation.json"), string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), 1234)
	if err != nil {
		t.Fatal(err)
	}
	return mode, privateKey
}

func repositoryActionContext(t *testing.T, environment, repository string) context.Context {
	t.Helper()
	metadata, err := auth.NewAuthenticationMetadataFromRaw(map[string]any{
		"public": map[string]any{"environment_id": environment, "repository": repository},
	})
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewContextWithAuthenticationMetadata(context.Background(), metadata)
}

func TestRepositoryActionIdentityAndStatefulBoundary(t *testing.T) {
	mode, _ := repositoryModeFixture(t)
	command := &model_command_pb.Command{NeedsWritableInputFiles: true, StableInputRootPathUuid: uuid.NewString()}
	for name, ctx := range map[string]context.Context{
		"no verified certificate metadata": context.Background(),
		"other environment":                repositoryActionContext(t, "env-b", "ductone/c1"),
		"other repository":                 repositoryActionContext(t, "env-a", "ductone/other"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := mode.validateCommand(ctx, command); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("unauthorized repository action: %v", err)
			}
		})
	}
	// Even with the matching public certificate claims, no issuer-provided
	// runtime attestation means no command can reach the runner.
	if err := mode.validateCommand(repositoryActionContext(t, "env-a", "ductone/c1"), command); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing signed isolation attestation: %v", err)
	}
	if err := mode.validateCommand(context.Background(), &model_command_pb.Command{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("generic action accepted by repository worker: %v", err)
	}
	if err := mode.validateCommand(context.Background(), &model_command_pb.Command{StableInputRootPathUuid: "../escape"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unsafe stable root accepted: %v", err)
	}
	if err := validateNativeCommand(command); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("generic worker accepted repository action: %v", err)
	}
}

func TestRepositoryAttestationCannotBeReplayedOrForged(t *testing.T) {
	mode, privateKey := repositoryModeFixture(t)
	now := time.Now()
	claims := isolationClaims{
		Environment: mode.Environment, Repository: mode.Repository,
		WorkerPID: os.Getpid(), RunnerPID: os.Getpid() + 1, RunnerUID: mode.RunnerUID,
		MountPath: mode.MountPath, RunnerSocket: mode.RunnerSocket,
		NetworkNamespace: "net:[1]", MountNamespace: "mnt:[1]", Cgroup: "/dedicated",
		ProxyEndpoint: "https://proxy.example/v1/repository-fetch", EgressPolicy: "proxy-only",
		FilesystemPolicy: "action-only", ProcessPolicy: "kill-descendants",
		IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute),
	}
	sign := func(c isolationClaims) {
		t.Helper()
		payload, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := json.Marshal(signedIsolationClaims{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mode.AttestationPath, envelope, 0600); err != nil {
			t.Fatal(err)
		}
	}
	sign(claims)
	// Valid cryptography is insufficient: the claimed namespace must match
	// the live worker, and no FUSE device exists in this local fixture.
	if err := mode.Verify(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "namespace mismatch") {
		t.Fatalf("signed but fabricated runtime unexpectedly accepted: %v", err)
	}
	for name, mutate := range map[string]func(*isolationClaims){
		"cross-environment":  func(c *isolationClaims) { c.Environment = "env-b" },
		"cross-repository":   func(c *isolationClaims) { c.Repository = "ductone/other" },
		"direct egress":      func(c *isolationClaims) { c.EgressPolicy = "direct" },
		"shared filesystem":  func(c *isolationClaims) { c.FilesystemPolicy = "host" },
		"orphan processes":   func(c *isolationClaims) { c.ProcessPolicy = "keep-descendants" },
		"expired":            func(c *isolationClaims) { c.ExpiresAt = now.Add(-time.Second) },
		"unbounded lifetime": func(c *isolationClaims) { c.ExpiresAt = now.Add(time.Hour) },
		"proxy exfil URL":    func(c *isolationClaims) { c.ProxyEndpoint = "https://user:token@evil.example/v1/repository-fetch" },
	} {
		t.Run(name, func(t *testing.T) {
			modified := claims
			mutate(&modified)
			sign(modified)
			if err := mode.Verify(); status.Code(err) != codes.FailedPrecondition ||
				!strings.Contains(err.Error(), "binding invalid or expired") ||
				strings.Contains(err.Error(), "token") {
				t.Fatalf("untrusted attestation accepted or secret exposed: %v", err)
			}
		})
	}
	sign(claims)
	data, err := os.ReadFile(mode.AttestationPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mode.AttestationPath, []byte(strings.Replace(string(data), "env-a", "env-b", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := mode.Verify(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "signature invalid") {
		t.Fatalf("tampered signature accepted: %v", err)
	}
}
