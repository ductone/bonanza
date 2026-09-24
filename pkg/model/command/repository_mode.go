package command

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	model_command_pb "bonanza.build/pkg/proto/model/command"

	"github.com/buildbarn/bb-storage/pkg/auth"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RepositoryMode belongs to a dedicated single-environment worker process.
// The operator's signing key is never loaded here: the worker holds only the
// public key and revalidates a short-lived isolation attestation per action.
// The signed egress claim requires independent operator verification of the
// runner's actual network policy; an action or scheduler cannot set it.
type RepositoryMode struct {
	Environment, Repository, MountPath, RunnerSocket, AttestationPath string
	Authority                                                         ed25519.PublicKey
	RunnerUID                                                         uint32
}

type isolationClaims struct {
	Environment      string    `json:"environment"`
	Repository       string    `json:"repository"`
	WorkerPID        int       `json:"worker_pid"`
	RunnerPID        int       `json:"runner_pid"`
	RunnerUID        uint32    `json:"runner_uid"`
	MountPath        string    `json:"mount_path"`
	RunnerSocket     string    `json:"runner_socket"`
	NetworkNamespace string    `json:"network_namespace"`
	MountNamespace   string    `json:"mount_namespace"`
	Cgroup           string    `json:"cgroup"`
	ProxyEndpoint    string    `json:"proxy_endpoint"`
	EgressPolicy     string    `json:"egress_policy"`
	FilesystemPolicy string    `json:"filesystem_policy"`
	ProcessPolicy    string    `json:"process_policy"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type signedIsolationClaims struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

func NewRepositoryMode(environment, repository, mountPath, runnerSocket, attestationPath, authorityPEM string, runnerUID uint32) (*RepositoryMode, error) {
	if environment == "" || repository == "" || mountPath == "" || runnerSocket == "" || attestationPath == "" || authorityPEM == "" || runnerUID == 0 {
		return nil, status.Error(codes.FailedPrecondition, "repository worker requires an environment, repository, FUSE mount, runner socket, attestation, signing authority, and unprivileged runner UID")
	}
	if !validEnvironment(environment) || !validRepository(repository) ||
		!filepath.IsAbs(mountPath) || !filepath.IsAbs(runnerSocket) || !filepath.IsAbs(attestationPath) ||
		filepath.Clean(mountPath) != mountPath || filepath.Clean(runnerSocket) != runnerSocket ||
		filepath.Clean(attestationPath) != attestationPath {
		return nil, status.Error(codes.FailedPrecondition, "repository worker binding is invalid")
	}
	for _, trustedPath := range []string{runnerSocket, attestationPath} {
		relative, err := filepath.Rel(mountPath, trustedPath)
		if err != nil || relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return nil, status.Error(codes.FailedPrecondition, "repository action mount cannot contain worker control files")
		}
	}
	block, rest := pem.Decode([]byte(authorityPEM))
	if block == nil || block.Type != "PUBLIC KEY" || len(rest) != 0 {
		return nil, status.Error(codes.FailedPrecondition, "repository worker signing authority must be one PEM public key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "repository worker signing authority is invalid")
	}
	publicKey, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "repository worker signing authority must use Ed25519")
	}
	return &RepositoryMode{
		Environment:     environment,
		Repository:      repository,
		MountPath:       mountPath,
		RunnerSocket:    runnerSocket,
		AttestationPath: attestationPath,
		Authority:       publicKey,
		RunnerUID:       runnerUID,
	}, nil
}

func validEnvironment(s string) bool {
	if strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validRepository(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

func (m *RepositoryMode) validateCommand(ctx context.Context, command *model_command_pb.Command) error {
	if !command.GetNeedsWritableInputFiles() && command.GetStableInputRootPathUuid() == "" {
		return status.Error(codes.FailedPrecondition, "repository worker does not execute generic build actions")
	}
	if id := command.GetStableInputRootPathUuid(); id != "" {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id {
			return status.Error(codes.InvalidArgument, "invalid stable input root UUID")
		}
	}
	identity := auth.AuthenticationMetadataFromContext(ctx).GetFullProto().GetPublic().GetStructValue()
	if identity == nil || identity.Fields["environment_id"].GetStringValue() != m.Environment || identity.Fields["repository"].GetStringValue() != m.Repository {
		return status.Error(codes.PermissionDenied, "repository action certificate metadata is missing or mismatched")
	}
	return m.Verify()
}

func (m *RepositoryMode) Verify() error {
	if m == nil {
		return status.Error(codes.FailedPrecondition, "repository worker is not configured")
	}
	pathInfo, err := os.Lstat(m.AttestationPath)
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm()&0022 != 0 {
		return status.Error(codes.FailedPrecondition, "repository isolation attestation must be an operator-owned regular file")
	}
	owner, ok := pathInfo.Sys().(*syscall.Stat_t)
	if !ok || (owner.Uid != 0 && owner.Uid != uint32(os.Getuid())) || owner.Uid == m.RunnerUID {
		return status.Error(codes.FailedPrecondition, "repository isolation attestation owner invalid")
	}
	file, err := os.Open(m.AttestationPath)
	if err != nil {
		return status.Error(codes.FailedPrecondition, "repository isolation attestation unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 8192 {
		return status.Error(codes.FailedPrecondition, "repository isolation attestation size invalid")
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		return status.Error(codes.FailedPrecondition, "repository isolation attestation unreadable")
	}
	var envelope signedIsolationClaims
	if json.Unmarshal(data, &envelope) != nil {
		return status.Error(codes.FailedPrecondition, "repository isolation attestation malformed")
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || !ed25519.Verify(m.Authority, envelope.Payload, signature) {
		return status.Error(codes.FailedPrecondition, "repository isolation attestation signature invalid")
	}
	var claims isolationClaims
	if json.Unmarshal(envelope.Payload, &claims) != nil {
		return status.Error(codes.FailedPrecondition, "repository isolation attestation payload invalid")
	}
	return m.verifyClaims(claims, time.Now())
}

func (m *RepositoryMode) verifyClaims(c isolationClaims, now time.Time) error {
	if c.Environment != m.Environment || c.Repository != m.Repository || c.WorkerPID != os.Getpid() ||
		c.RunnerPID <= 1 || c.RunnerUID != m.RunnerUID || c.MountPath != m.MountPath ||
		c.RunnerSocket != m.RunnerSocket || c.EgressPolicy != "proxy-only" ||
		c.FilesystemPolicy != "action-only" || c.ProcessPolicy != "kill-descendants" ||
		!validProxyEndpoint(c.ProxyEndpoint) || c.IssuedAt.After(now) ||
		!now.Before(c.ExpiresAt) || c.ExpiresAt.Sub(c.IssuedAt) > 10*time.Minute {
		return status.Error(codes.FailedPrecondition, "repository isolation attestation binding invalid or expired")
	}
	if os.Getuid() == 0 || uint32(os.Getuid()) == m.RunnerUID || c.RunnerPID == c.WorkerPID {
		return status.Error(codes.FailedPrecondition, "repository worker and runner require separate unprivileged identities")
	}
	for _, namespace := range []struct{ name, claim string }{{"net", c.NetworkNamespace}, {"mnt", c.MountNamespace}} {
		worker, err := os.Readlink("/proc/self/ns/" + namespace.name)
		if err != nil || worker == "" || worker != namespace.claim {
			return status.Error(codes.FailedPrecondition, "repository worker namespace mismatch")
		}
		runner, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", c.RunnerPID, namespace.name))
		if err != nil || runner != worker {
			return status.Error(codes.FailedPrecondition, "repository runner namespace mismatch")
		}
	}
	workerCgroup, err := processCgroup("self")
	if err != nil || workerCgroup == "/" || workerCgroup != c.Cgroup {
		return status.Error(codes.FailedPrecondition, "repository worker cgroup mismatch")
	}
	runnerCgroup, err := processCgroup(strconv.Itoa(c.RunnerPID))
	if err != nil || runnerCgroup != workerCgroup {
		return status.Error(codes.FailedPrecondition, "repository runner cgroup mismatch")
	}
	if err := requireBoundedCgroup(workerCgroup); err != nil {
		return err
	}
	if err := requireRestrictedRunner(c.RunnerPID, m.RunnerUID); err != nil {
		return err
	}
	if err := requireRunnerSocket(m.RunnerSocket, m.RunnerUID); err != nil {
		return err
	}
	if err := requireFuseMount(m.MountPath); err != nil {
		return err
	}
	return nil
}

func validProxyEndpoint(s string) bool {
	endpoint, err := url.Parse(s)
	return err == nil && endpoint.Scheme == "https" && endpoint.Hostname() != "" && endpoint.User == nil &&
		endpoint.Path == "/v1/repository-fetch" && endpoint.RawPath == "" && endpoint.RawQuery == "" &&
		endpoint.Fragment == "" && endpoint.Opaque == "" && !strings.ContainsAny(s, "\\\r\n")
}

func processCgroup(pid string) (string, error) {
	data, err := os.ReadFile("/proc/" + pid + "/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::/") {
			return strings.TrimPrefix(line, "0::"), nil
		}
	}
	return "", errors.New("cgroup v2 required")
}

func requireBoundedCgroup(group string) error {
	if group == "/" || !filepath.IsAbs(group) || filepath.Clean(group) != group || strings.Contains(group, "..") {
		return status.Error(codes.FailedPrecondition, "dedicated cgroup v2 required")
	}
	for _, name := range []string{"cpu.max", "memory.max", "pids.max"} {
		data, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(group, "/"), name))
		if err != nil {
			return status.Error(codes.FailedPrecondition, "bounded repository cgroup required")
		}
		limits := strings.Fields(string(data))
		if len(limits) == 0 || limits[0] == "max" || limits[0] == "0" {
			return status.Error(codes.FailedPrecondition, "bounded repository cgroup required")
		}
	}
	return nil
}

func requireRestrictedRunner(pid int, uid uint32) error {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return status.Error(codes.FailedPrecondition, "repository runner unavailable")
	}
	fields := map[string][]string{}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) > 1 {
			fields[strings.TrimSuffix(parts[0], ":")] = parts[1:]
		}
	}
	if len(fields["Uid"]) != 4 || len(fields["NoNewPrivs"]) == 0 || len(fields["Seccomp"]) == 0 || len(fields["CapEff"]) == 0 || len(fields["CapBnd"]) == 0 ||
		fields["Uid"][0] != strconv.FormatUint(uint64(uid), 10) || fields["Uid"][1] != fields["Uid"][0] ||
		fields["NoNewPrivs"][0] != "1" || fields["Seccomp"][0] != "2" ||
		fields["CapEff"][0] != "0000000000000000" || fields["CapBnd"][0] != "0000000000000000" {
		return status.Error(codes.FailedPrecondition, "repository runner privilege isolation unavailable")
	}
	return nil
}

func requireRunnerSocket(socket string, uid uint32) error {
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0007 != 0 {
		return status.Error(codes.FailedPrecondition, "repository runner socket isolation unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid {
		return status.Error(codes.FailedPrecondition, "repository runner socket owner mismatch")
	}
	return nil
}

func requireFuseMount(mountPath string) error {
	info, err := os.Stat("/dev/fuse")
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return status.Error(codes.FailedPrecondition, "FUSE device unavailable")
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return status.Error(codes.FailedPrecondition, "FUSE mount unavailable")
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) == 2 && len(strings.Fields(parts[0])) >= 5 && strings.Fields(parts[0])[4] == mountPath && strings.HasPrefix(parts[1], "fuse.") {
			return nil
		}
	}
	return status.Error(codes.FailedPrecondition, "dedicated FUSE mount unavailable")
}
