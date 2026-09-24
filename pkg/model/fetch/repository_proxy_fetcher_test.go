package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	model_fetch_pb "bonanza.build/pkg/proto/model/fetch"

	"github.com/buildbarn/bb-storage/pkg/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type repositoryProxyRoundTripper func(*http.Request) (*http.Response, error)

func (f repositoryProxyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func certificateIdentity(t *testing.T, environment, repository string) context.Context {
	t.Helper()
	metadata, err := auth.NewAuthenticationMetadataFromRaw(map[string]any{
		"public": map[string]any{"environment_id": environment, "repository": repository},
	})
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewContextWithAuthenticationMetadata(context.Background(), metadata)
}

func TestRepositoryProxyFetchSendsOnlyURL(t *testing.T) {
	const target = "https://github.com/ductone/c1/archive/refs/tags/v1.tar.gz"
	calls := 0
	client := &http.Client{Transport: repositoryProxyRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodPost || req.URL.String() != "https://proxy.example/v1/repository-fetch" {
			t.Errorf("unexpected proxy request %s %s", req.Method, req.URL)
		}
		payload, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(payload) != `{"url":"`+target+`"}` {
			t.Errorf("proxy request contains unexpected fields: %s", payload)
		}
		if !reflect.DeepEqual(req.Header, http.Header{"Content-Type": {"application/json"}}) {
			t.Errorf("unexpected proxy request headers: %v", req.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("archive")), Header: http.Header{}}, nil
	})}
	fetcher, err := NewRepositoryProxyFetcher(client, "https://proxy.example/v1/repository-fetch", "env-a", "ductone/c1")
	if err != nil {
		t.Fatal(err)
	}
	body, err := fetcher.Fetch(certificateIdentity(t, "env-a", "ductone/c1"), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	contents, err := io.ReadAll(body)
	if err != nil || string(contents) != "archive" || calls != 1 {
		t.Fatalf("fetch result = %q, %v; calls = %d", contents, err, calls)
	}
}

func TestRepositoryProxyRejectsUntrustedRequestsBeforeNetwork(t *testing.T) {
	client := &http.Client{Transport: repositoryProxyRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("untrusted request reached proxy")
		return nil, nil
	})}
	fetcher, err := NewRepositoryProxyFetcher(client, "https://proxy.example/v1/repository-fetch", "env-a", "ductone/c1")
	if err != nil {
		t.Fatal(err)
	}
	allowed := "https://github.com/ductone/c1/archive/v1.tar.gz"
	for name, testcase := range map[string]struct {
		ctx     context.Context
		url     string
		headers []*model_fetch_pb.Target_Header
	}{
		"missing identity":    {ctx: context.Background(), url: allowed},
		"wrong environment":   {ctx: certificateIdentity(t, "env-b", "ductone/c1"), url: allowed},
		"wrong repository":    {ctx: certificateIdentity(t, "env-a", "ductone/other"), url: allowed},
		"credential header":   {ctx: certificateIdentity(t, "env-a", "ductone/c1"), url: allowed, headers: []*model_fetch_pb.Target_Header{{Name: "Authorization", Value: "secret"}}},
		"cross-repository":    {ctx: certificateIdentity(t, "env-a", "ductone/c1"), url: "https://github.com/ductone/other/archive/v1.tar.gz"},
		"prefix confusion":    {ctx: certificateIdentity(t, "env-a", "ductone/c1"), url: "https://github.com/ductone/c1-evil/archive/v1.tar.gz"},
		"userinfo":            {ctx: certificateIdentity(t, "env-a", "ductone/c1"), url: "https://github.com@evil.example/ductone/c1/archive"},
		"other host":          {ctx: certificateIdentity(t, "env-a", "ductone/c1"), url: "https://raw.githubusercontent.com/ductone/c1/file"},
		"plain HTTP":          {ctx: certificateIdentity(t, "env-a", "ductone/c1"), url: "http://github.com/ductone/c1/archive"},
		"encoded traversal":   {ctx: certificateIdentity(t, "env-a", "ductone/c1"), url: "https://github.com/ductone/c1/%2e%2e/other/archive"},
		"double encoded path": {ctx: certificateIdentity(t, "env-a", "ductone/c1"), url: "https://github.com/ductone/c1/%252e%252e/other/archive"},
		"query token":         {ctx: certificateIdentity(t, "env-a", "ductone/c1"), url: allowed + "?token=secret"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := fetcher.Fetch(testcase.ctx, testcase.url, testcase.headers)
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("Fetch() error = %v, want PermissionDenied", err)
			}
		})
	}
}

func TestRepositoryProxyNeverFollowsRedirectOrEchoesProxyError(t *testing.T) {
	ctx := certificateIdentity(t, "env-a", "ductone/c1")
	url := "https://github.com/ductone/c1/archive/v1.tar.gz"
	for name, response := range map[string]*http.Response{
		"redirect": {StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://evil.example/token"}}, Body: io.NopCloser(strings.NewReader("secret"))},
		"error":    {StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("private-token")), Header: http.Header{}},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: repositoryProxyRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return response, nil
			})}
			fetcher, err := NewRepositoryProxyFetcher(client, "https://proxy.example/v1/repository-fetch", "env-a", "ductone/c1")
			if err != nil {
				t.Fatal(err)
			}
			body, err := fetcher.Fetch(ctx, url, nil)
			if err == nil || body != nil || calls != 1 || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token") {
				t.Fatalf("Fetch() = %v, %v with %d proxy requests", body, err, calls)
			}
		})
	}
}

func TestRepositoryProxyMissingDependencyFailsClosed(t *testing.T) {
	if _, err := NewRepositoryProxyFetcher(nil, "https://proxy.example/v1/repository-fetch", "env-a", "ductone/c1"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing transport: %v", err)
	}
	for _, endpoint := range []string{"", "http://proxy.example/v1/repository-fetch", "https://user:secret@proxy.example/v1/repository-fetch", "https://proxy.example/other"} {
		if _, err := NewRepositoryProxyFetcher(&http.Client{Transport: repositoryProxyRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("down") })}, endpoint, "env-a", "ductone/c1"); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("endpoint %q unexpectedly accepted: %v", endpoint, err)
		}
	}
	fetcher, err := NewRepositoryProxyFetcher(&http.Client{Transport: repositoryProxyRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("down") })}, "https://proxy.example/v1/repository-fetch", "env-a", "ductone/c1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetcher.Fetch(certificateIdentity(t, "env-a", "ductone/c1"), "https://github.com/ductone/c1/archive/v1.tar.gz", nil); status.Code(err) != codes.Unavailable {
		t.Fatalf("missing proxy: %v", err)
	}
}

func TestRepositoryProxyRejectsOversizedResponse(t *testing.T) {
	client := &http.Client{Transport: repositoryProxyRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: maximumRepositoryFetchBytes + 1,
			Body:          io.NopCloser(strings.NewReader("not a bounded archive")),
		}, nil
	})}
	fetcher, err := NewRepositoryProxyFetcher(client, "https://proxy.example/v1/repository-fetch", "env-a", "ductone/c1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetcher.Fetch(certificateIdentity(t, "env-a", "ductone/c1"), "https://github.com/ductone/c1/archive/v1.tar.gz", nil)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized proxy response = %v, want ResourceExhausted", err)
	}
}
