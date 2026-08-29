package kubernetes

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"k8s.io/client-go/rest"
)

const (
	eksClusterIDHeader = "x-k8s-aws-id"
	eksTokenPrefix     = "k8s-aws-v1."
	eksTokenLifetime   = 15 * time.Minute
	eksRefreshMargin   = time.Minute
)

type eksTokenSource interface {
	Token(context.Context) (string, error)
}

type eksTokenValue struct {
	bearer    string
	expiresAt time.Time
}

type eksTokenCache struct {
	generate func(context.Context) (eksTokenValue, error)
	now      func() time.Time

	mutex sync.Mutex
	value eksTokenValue
}

func loadEKSConnectionConfig(ctx context.Context, config ConnectionConfig) (*rest.Config, error) {
	options := make([]func(*awsconfig.LoadOptions) error, 0, 1)
	if config.AWSRegion != "" {
		options = append(options, awsconfig.WithRegion(config.AWSRegion))
	}

	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}

	if awsConfig.Region == "" {
		return nil, errors.New("--aws-region is required when the AWS default configuration has no region")
	}

	if config.AWSRoleARN != "" {
		provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(awsConfig), config.AWSRoleARN)
		awsConfig.Credentials = aws.NewCredentialsCache(provider)
	}

	describer := eks.NewFromConfig(awsConfig)
	output, err := describer.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(config.EKSCluster)})
	if err != nil {
		return nil, fmt.Errorf("describe EKS cluster %s: %w", config.EKSCluster, err)
	}

	return eksRESTConfig(awsConfig, config.EKSCluster, output)
}

func eksRESTConfig(awsConfig aws.Config, clusterName string, output *eks.DescribeClusterOutput) (*rest.Config, error) {
	if output == nil || output.Cluster == nil || output.Cluster.Endpoint == nil || strings.TrimSpace(*output.Cluster.Endpoint) == "" {
		return nil, fmt.Errorf("describe EKS cluster %s: response has no endpoint", clusterName)
	}

	if output.Cluster.CertificateAuthority == nil || output.Cluster.CertificateAuthority.Data == nil {
		return nil, fmt.Errorf("describe EKS cluster %s: response has no certificate authority", clusterName)
	}

	certificateAuthority, err := base64.StdEncoding.DecodeString(*output.Cluster.CertificateAuthority.Data)
	if err != nil {
		return nil, fmt.Errorf("decode EKS cluster %s certificate authority: %w", clusterName, err)
	}

	tokens := newEKSTokenCache(newEKSTokenGenerator(awsConfig, clusterName), time.Now)

	return &rest.Config{
		Host: *output.Cluster.Endpoint,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: certificateAuthority,
		},
		WrapTransport: func(base http.RoundTripper) http.RoundTripper {
			return eksAuthorizationTransport{base: base, tokens: tokens}
		},
	}, nil
}

func newEKSTokenGenerator(awsConfig aws.Config, clusterName string) func(context.Context) (eksTokenValue, error) {
	client := sts.NewPresignClient(sts.NewFromConfig(awsConfig), func(options *sts.PresignOptions) {
		options.ClientOptions = append(options.ClientOptions, func(options *sts.Options) {
			options.APIOptions = append(options.APIOptions, addEKSClusterIDHeader(clusterName))
		})
	})

	return func(ctx context.Context) (eksTokenValue, error) {
		request, err := client.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			return eksTokenValue{}, fmt.Errorf("sign EKS authentication token: %w", err)
		}

		now := time.Now()

		return eksTokenValue{
			bearer:    eksTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(request.URL)),
			expiresAt: now.Add(eksTokenLifetime),
		}, nil
	}
}

func addEKSClusterIDHeader(clusterName string) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Build.Add(middleware.BuildMiddlewareFunc("AirlockEKSClusterID", func(
			ctx context.Context,
			input middleware.BuildInput,
			next middleware.BuildHandler,
		) (middleware.BuildOutput, middleware.Metadata, error) {
			request, ok := input.Request.(*smithyhttp.Request)
			if !ok {
				return middleware.BuildOutput{}, middleware.Metadata{}, errors.New("sign EKS authentication token: unexpected request type")
			}

			request.Header.Set(eksClusterIDHeader, clusterName)

			return next.HandleBuild(ctx, input)
		}), middleware.After)
	}
}

func newEKSTokenCache(generate func(context.Context) (eksTokenValue, error), now func() time.Time) *eksTokenCache {
	return &eksTokenCache{generate: generate, now: now}
}

func (cache *eksTokenCache) Token(ctx context.Context) (string, error) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	if cache.value.bearer != "" && cache.now().Add(eksRefreshMargin).Before(cache.value.expiresAt) {
		return cache.value.bearer, nil
	}

	value, err := cache.generate(ctx)
	if err != nil {
		return "", err
	}

	cache.value = value

	return value.bearer, nil
}

type eksAuthorizationTransport struct {
	base   http.RoundTripper
	tokens eksTokenSource
}

func (transport eksAuthorizationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	token, err := transport.tokens.Token(request.Context())
	if err != nil {
		return nil, err
	}

	authenticated := request.Clone(request.Context())
	authenticated.Header = request.Header.Clone()
	authenticated.Header.Set("Authorization", "Bearer "+token)

	return transport.base.RoundTrip(authenticated)
}
