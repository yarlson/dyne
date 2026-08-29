package kubernetes

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEKSTokenGeneratorSignsTheClusterID(t *testing.T) {
	generator := newEKSTokenGenerator(aws.Config{
		Region: "eu-west-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			"AKIDEXAMPLE",
			"secret",
			"session",
		),
	}, "production")

	value, err := generator(context.Background())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(value.bearer, "k8s-aws-v1."))
	assert.WithinDuration(t, time.Now().Add(15*time.Minute), value.expiresAt, time.Minute)

	encodedURL := strings.TrimPrefix(value.bearer, "k8s-aws-v1.")
	contents, err := base64.RawURLEncoding.DecodeString(encodedURL)
	require.NoError(t, err)
	signedURL, err := url.Parse(string(contents))
	require.NoError(t, err)

	assert.Equal(t, "GetCallerIdentity", signedURL.Query().Get("Action"))
	assert.Equal(t, "2011-06-15", signedURL.Query().Get("Version"))
	assert.Contains(t, strings.Split(signedURL.Query().Get("X-Amz-SignedHeaders"), ";"), "x-k8s-aws-id")
}

func TestEKSTokenCacheSharesAnUnexpiredToken(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	cache := newEKSTokenCache(func(context.Context) (eksTokenValue, error) {
		calls.Add(1)

		return eksTokenValue{bearer: "shared", expiresAt: now.Add(15 * time.Minute)}, nil
	}, func() time.Time { return now })

	const callers = 20
	results := make(chan string, callers)
	var group sync.WaitGroup
	for range callers {
		group.Go(func() {
			token, err := cache.Token(context.Background())
			assert.NoError(t, err)
			results <- token
		})
	}

	group.Wait()
	close(results)

	for token := range results {
		assert.Equal(t, "shared", token)
	}

	assert.Equal(t, int32(1), calls.Load())
}

func TestEKSTokenCacheRefreshesBeforeExpiry(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	calls := 0
	cache := newEKSTokenCache(func(context.Context) (eksTokenValue, error) {
		calls++

		return eksTokenValue{
			bearer:    []string{"first", "second"}[calls-1],
			expiresAt: now.Add(15 * time.Minute),
		}, nil
	}, func() time.Time { return now })

	first, err := cache.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "first", first)

	now = now.Add(14*time.Minute + time.Second)
	second, err := cache.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "second", second)
	assert.Equal(t, 2, calls)
}

func TestEKSAuthorizationTransportPreservesTheInputRequest(t *testing.T) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://cluster.example/api", nil)
	require.NoError(t, err)
	request.Header.Set("X-Request", "original")

	transport := eksAuthorizationTransport{
		base: roundTripperFunc(func(got *http.Request) (*http.Response, error) {
			assert.Equal(t, "Bearer signed-token", got.Header.Get("Authorization"))
			assert.Equal(t, "original", got.Header.Get("X-Request"))

			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
		tokens: staticEKSTokenSource("signed-token"),
	}

	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Empty(t, request.Header.Get("Authorization"))
}

func TestEKSRESTConfigUsesDiscoveredEndpointAndCertificate(t *testing.T) {
	awsConfig := aws.Config{
		Region:      "eu-west-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "session"),
	}
	output := &eks.DescribeClusterOutput{Cluster: &types.Cluster{
		Endpoint: aws.String("https://cluster.example"),
		CertificateAuthority: &types.Certificate{
			Data: aws.String(base64.StdEncoding.EncodeToString([]byte("cluster certificate"))),
		},
	}}

	config, err := eksRESTConfig(awsConfig, "production", output)
	require.NoError(t, err)
	assert.Equal(t, "https://cluster.example", config.Host)
	assert.Equal(t, []byte("cluster certificate"), config.CAData)

	transport := config.WrapTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		assert.True(t, strings.HasPrefix(request.Header.Get("Authorization"), "Bearer k8s-aws-v1."))

		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}))
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, config.Host, nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
}

func TestEKSRESTConfigRejectsIncompleteDiscovery(t *testing.T) {
	awsConfig := aws.Config{Region: "eu-west-1"}
	cases := []struct {
		name   string
		output *eks.DescribeClusterOutput
		want   string
	}{
		{name: "missing cluster", want: "describe EKS cluster production: response has no endpoint"},
		{name: "missing endpoint", output: &eks.DescribeClusterOutput{Cluster: &types.Cluster{}}, want: "describe EKS cluster production: response has no endpoint"},
		{
			name: "missing certificate",
			output: &eks.DescribeClusterOutput{Cluster: &types.Cluster{
				Endpoint: aws.String("https://cluster.example"),
			}},
			want: "describe EKS cluster production: response has no certificate authority",
		},
		{
			name: "invalid certificate",
			output: &eks.DescribeClusterOutput{Cluster: &types.Cluster{
				Endpoint:             aws.String("https://cluster.example"),
				CertificateAuthority: &types.Certificate{Data: aws.String("not base64")},
			}},
			want: "decode EKS cluster production certificate authority",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := eksRESTConfig(awsConfig, "production", test.output)
			require.ErrorContains(t, err, test.want)
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type staticEKSTokenSource string

func (source staticEKSTokenSource) Token(context.Context) (string, error) {
	return string(source), nil
}
