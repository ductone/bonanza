package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	model_fetch_pb "bonanza.build/pkg/proto/model/fetch"

	"github.com/buildbarn/bb-storage/pkg/auth"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maximumRepositoryFetchBytes = 256 << 20
	repositoryFetchTimeout      = 60 * time.Second
)

// repositoryProxyFetcher sends only an allow-listed URL to a trusted proxy.
// The proxy derives environment and repository authorization from the client's
// mTLS identity; neither action data nor this fetcher supplies identity claims.
// Its caller must provide an mTLS-authenticated HTTP transport and must not
// mount its client certificate or any Git credential inside action processes.
type repositoryProxyFetcher struct {
	client      http.Client
	endpoint    string
	environment string
	repository  string
}

// NewRepositoryProxyFetcher replaces direct HTTP(S) fetching on a dedicated
// per-environment fetcher. There is deliberately no fallback to direct HTTP.
func NewRepositoryProxyFetcher(client *http.Client, endpoint, environment, repository string) (Fetcher, error) {
	if client == nil || client.Transport == nil {
		return nil, status.Error(codes.FailedPrecondition, "repository fetch proxy requires an authenticated transport")
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Scheme != "https" || parsedEndpoint.Hostname() == "" || parsedEndpoint.User != nil || parsedEndpoint.Fragment != "" || parsedEndpoint.RawQuery != "" || parsedEndpoint.Opaque != "" || parsedEndpoint.Path != "/v1/repository-fetch" {
		return nil, status.Error(codes.InvalidArgument, "invalid repository fetch proxy endpoint")
	}
	if environment == "" || strings.TrimSpace(environment) != environment {
		return nil, status.Error(codes.InvalidArgument, "repository fetch proxy requires a verified environment binding")
	}
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || !validGitHubName(parts[0]) || !validGitHubName(parts[1]) {
		return nil, status.Error(codes.InvalidArgument, "repository fetch proxy requires a GitHub owner/repository allowlist")
	}
	proxyClient := *client
	proxyClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("repository fetch proxy redirect denied")
	}
	if proxyClient.Timeout == 0 || proxyClient.Timeout > repositoryFetchTimeout {
		proxyClient.Timeout = repositoryFetchTimeout
	}
	return &repositoryProxyFetcher{client: proxyClient, endpoint: endpoint, environment: environment, repository: repository}, nil
}

func validGitHubName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, c := range name {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (f *repositoryProxyFetcher) Fetch(ctx context.Context, target string, headers []*model_fetch_pb.Target_Header) (io.ReadCloser, error) {
	// remoteworker attaches metadata extracted by its X.509 verifier. The
	// empty metadata of the demo verifier cannot authorize a fetch.
	identity := auth.AuthenticationMetadataFromContext(ctx).GetFullProto().GetPublic().GetStructValue()
	if identity == nil ||
		identity.Fields["environment_id"].GetStringValue() != f.environment ||
		identity.Fields["repository"].GetStringValue() != f.repository {
		return nil, status.Error(codes.PermissionDenied, "repository fetch client identity is missing or mismatched")
	}
	if len(headers) != 0 {
		return nil, status.Error(codes.PermissionDenied, "repository fetch action headers are not accepted")
	}
	parsedTarget, err := url.Parse(target)
	if err != nil || parsedTarget.Scheme != "https" || !strings.EqualFold(parsedTarget.Host, "github.com") || parsedTarget.User != nil || parsedTarget.Fragment != "" || parsedTarget.RawQuery != "" || parsedTarget.RawPath != "" || parsedTarget.Opaque != "" || strings.Contains(target, "%") {
		return nil, status.Error(codes.PermissionDenied, "repository fetch URL is not allowed")
	}
	components := strings.Split(strings.TrimPrefix(parsedTarget.Path, "/"), "/")
	allowed := strings.Split(f.repository, "/")
	if !strings.HasPrefix(parsedTarget.Path, "/") || len(components) < 3 || !strings.EqualFold(components[0], allowed[0]) || !strings.EqualFold(components[1], allowed[1]) {
		return nil, status.Error(codes.PermissionDenied, "repository fetch URL is outside the allowed repository")
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." || strings.ContainsAny(component, "\\\r\n") {
			return nil, status.Error(codes.PermissionDenied, "repository fetch URL has an invalid path")
		}
	}
	payload, err := json.Marshal(struct {
		URL string `json:"url"`
	}{URL: target})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid repository fetch URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to create repository proxy request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "repository fetch proxy unavailable")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			return nil, status.Error(codes.PermissionDenied, "repository fetch denied by proxy")
		}
		return nil, status.Error(codes.Unavailable, "repository fetch proxy did not return a file")
	}
	if resp.ContentLength > maximumRepositoryFetchBytes {
		resp.Body.Close()
		return nil, status.Error(codes.ResourceExhausted, "repository fetch exceeds size limit")
	}
	return &boundedRepositoryBody{body: resp.Body}, nil
}

type boundedRepositoryBody struct {
	body      io.ReadCloser
	readBytes int64
}

func (b *boundedRepositoryBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if remaining := maximumRepositoryFetchBytes - b.readBytes; int64(len(p)) > remaining+1 {
		p = p[:int(remaining+1)]
	}
	n, err := b.body.Read(p)
	b.readBytes += int64(n)
	if b.readBytes > maximumRepositoryFetchBytes {
		b.body.Close()
		return n, status.Error(codes.ResourceExhausted, "repository fetch exceeds size limit")
	}
	return n, err
}

func (b *boundedRepositoryBody) Close() error { return b.body.Close() }
