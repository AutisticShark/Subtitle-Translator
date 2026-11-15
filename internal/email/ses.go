package email

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

var sesRegionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]+$`)

// sesTransport uses the official AWS SDK for Go v2 SES v2 SendEmail operation.
// The SDK does the Signature V4 signing, session tokens, endpoint resolution and
// the credential chain (environment, shared files, container and IMDS roles).
// Requests are attempted once, with no SDK retries, and endpoint URLs configured
// through AWS_ENDPOINT_URL* or shared config are ignored so credentials only ever
// go to the region's regular SES endpoint. A custom CA bundle (AWS_CA_BUNDLE or
// a profile's ca_bundle) is honoured as boto3 did, through the SDK's own loader.
type sesTransport struct {
	id        identity
	region    string
	configSet string

	// Static credentials, only when both are set in the configuration map.
	// Otherwise the SDK's default chain applies.
	accessKey, secretKey, sessionToken string

	// Test hooks. roundTripper replaces the network for SES requests;
	// adjustTransport edits the SDK-built transport (keeping its TLS roots),
	// for example to dial a local server.
	roundTripper    http.RoundTripper
	adjustTransport func(*http.Transport)
}

func newSES(env map[string]string, id identity) (transport, error) {
	region := firstNonEmpty(env["AWS_SES_REGION"], env["AWS_REGION"], env["AWS_DEFAULT_REGION"])
	region = strings.TrimSpace(region)
	if !sesRegionPattern.MatchString(region) {
		return nil, configError("AWS_SES_REGION is required and must be a valid region")
	}
	accessKey := strings.TrimSpace(env["AWS_ACCESS_KEY_ID"]) != ""
	secretKey := strings.TrimSpace(env["AWS_SECRET_ACCESS_KEY"]) != ""
	if accessKey != secretKey || (env["AWS_SESSION_TOKEN"] != "" && !accessKey) {
		return nil, configError("AWS environment credentials require both access key ID and secret access key")
	}
	t := &sesTransport{id: id, region: region, configSet: strings.TrimSpace(env["AWS_SES_CONFIGURATION_SET"])}
	if accessKey {
		t.accessKey = strings.TrimSpace(env["AWS_ACCESS_KEY_ID"])
		t.secretKey = strings.TrimSpace(env["AWS_SECRET_ACCESS_KEY"])
		t.sessionToken = env["AWS_SESSION_TOKEN"]
	}
	return t, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *sesTransport) deliver(ctx context.Context, m message) error {
	// The SDK applies a custom CA bundle only to its BuildableClient, which
	// also serves the credential providers. Keep-alives are off: the config
	// is loaded per send (as boto3's session was), so pooled connections
	// would only linger.
	buildable := awshttp.NewBuildableClient().WithTimeout(timeout).WithTransportOptions(func(tr *http.Transport) {
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		}
		tr.TLSClientConfig.MinVersion = tls.VersionTLS12
		tr.DisableKeepAlives = true
	})
	options := []func(*config.LoadOptions) error{
		config.WithRegion(s.region),
		config.WithHTTPClient(buildable),
		config.WithRetryMaxAttempts(1),
		config.WithRetryer(func() aws.Retryer { return aws.NopRetryer{} }),
	}
	if s.accessKey != "" {
		options = append(options, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(s.accessKey, s.secretKey, s.sessionToken)))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return err
	}
	// SendEmail itself goes through a client that never follows redirects
	// (the BuildableClient follows 307/308), built on the transport the SDK
	// configured, custom CA bundle included.
	rt := s.roundTripper
	if rt == nil {
		configured, ok := cfg.HTTPClient.(*awshttp.BuildableClient)
		if !ok {
			return errors.New("unexpected AWS HTTP client")
		}
		tr := configured.GetTransport()
		if s.adjustTransport != nil {
			s.adjustTransport(tr)
		}
		rt = tr
	}
	client := newHTTPClient(rt)
	defer client.CloseIdleConnections()
	api := sesv2.NewFromConfig(cfg, func(o *sesv2.Options) {
		o.HTTPClient = client
		o.BaseEndpoint = nil // ignore AWS_ENDPOINT_URL and shared config endpoint URLs
		o.Retryer = aws.NopRetryer{}
	})

	body := &types.Body{Text: utf8Content(m.text)}
	if m.html != "" {
		body.Html = utf8Content(m.html)
	}
	input := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.id.apiValue()),
		Destination:      &types.Destination{ToAddresses: []string{m.to}},
		Content: &types.EmailContent{Simple: &types.Message{
			Subject: utf8Content(m.subject),
			Body:    body,
		}},
	}
	if s.configSet != "" {
		input.ConfigurationSetName = aws.String(s.configSet)
	}
	out, err := api.SendEmail(ctx, input)
	if err != nil {
		return err
	}
	if out == nil || aws.ToString(out.MessageId) == "" {
		return errors.New("SES returned no message id")
	}
	return nil
}

func utf8Content(data string) *types.Content {
	return &types.Content{Data: aws.String(data), Charset: aws.String("UTF-8")}
}
