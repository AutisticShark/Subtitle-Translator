package email

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// These tests drive the real AWS SDK for Go v2 (signing, endpoint resolution,
// serialization, retry policy) against a mocked HTTP transport.

// isolateAWS keeps the developer's AWS configuration out of the tests and makes
// any attempt to reach instance metadata fail fast.
func isolateAWS(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for name, value := range map[string]string{
		"AWS_CONFIG_FILE":                        filepath.Join(dir, "config"),
		"AWS_SHARED_CREDENTIALS_FILE":            filepath.Join(dir, "credentials"),
		"AWS_EC2_METADATA_DISABLED":              "true",
		"AWS_CA_BUNDLE":                          "",
		"AWS_ACCESS_KEY_ID":                      "",
		"AWS_SECRET_ACCESS_KEY":                  "",
		"AWS_SESSION_TOKEN":                      "",
		"AWS_PROFILE":                            "",
		"AWS_ENDPOINT_URL":                       "",
		"AWS_ENDPOINT_URL_SESV2":                 "",
		"AWS_IGNORE_CONFIGURED_ENDPOINT_URLS":    "",
		"AWS_REGION":                             "",
		"AWS_DEFAULT_REGION":                     "",
		"AWS_WEB_IDENTITY_TOKEN_FILE":            "",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":     "",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "",
	} {
		t.Setenv(name, value)
	}
}

func sesWithStub(t *testing.T, env map[string]string, respond func(int, *http.Request) (*http.Response, error)) (service, *stubTransport) {
	t.Helper()
	svc := mustNew(t, env)
	stub := &stubTransport{respond: respond}
	svc.t.(*sesTransport).roundTripper = stub
	return svc, stub
}

func sesEnv(extra map[string]string) map[string]string {
	env := baseEnv("ses", map[string]string{
		"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "private-secret",
	})
	for key, value := range extra {
		env[key] = value
	}
	return env
}

func TestSESSDKContract(t *testing.T) {
	isolateAWS(t)
	svc, stub := sesWithStub(t, sesEnv(map[string]string{
		"AWS_SES_CONFIGURATION_SET": "transactional", "AWS_SESSION_TOKEN": "private-token"}),
		jsonReply(200, `{"MessageId":"accepted"}`))
	if err := svc.Send(context.Background(), "user@example.com", "Subject 驗證", "Text", "<p>HTML</p>"); err != nil {
		t.Fatal(err)
	}
	req := stub.reqs[0]
	if req.Method != http.MethodPost || req.URL.String() != "https://email.us-east-1.amazonaws.com/v2/email/outbound-emails" {
		t.Fatalf("request %s %s", req.Method, req.URL)
	}
	auth := req.Header.Get("Authorization")
	if !regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/\d{8}/us-east-1/ses/aws4_request, SignedHeaders=.*host.*, Signature=[0-9a-f]{64}$`).MatchString(auth) {
		t.Fatalf("not a Signature V4 header: %q", auth)
	}
	if req.Header.Get("X-Amz-Security-Token") != "private-token" {
		t.Fatalf("session token header %q", req.Header.Get("X-Amz-Security-Token"))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stub.bodies[0]), &got); err != nil {
		t.Fatal(err)
	}
	text := func(data string) map[string]any { return map[string]any{"Data": data, "Charset": "UTF-8"} }
	want := map[string]any{
		"FromEmailAddress": "Mail <sender@example.com>",
		"Destination":      map[string]any{"ToAddresses": []any{"user@example.com"}},
		"Content": map[string]any{"Simple": map[string]any{
			"Subject": text("Subject 驗證"),
			"Body":    map[string]any{"Text": text("Text"), "Html": text("<p>HTML</p>")},
		}},
		"ConfigurationSetName": "transactional",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SendEmail body:\n got %v\nwant %v", got, want)
	}
}

// A long non-ASCII display name needs several encoded words. In the SMTP header
// they are folded onto continuation lines, but SES takes FromEmailAddress as one
// field and must never receive a line break in it.
func TestSESLongNonASCIIDisplayNameHasNoLineBreak(t *testing.T) {
	isolateAWS(t)
	name := "Flawless 字幕翻譯服務通知中心驗證碼寄送"
	svc, stub := sesWithStub(t, sesEnv(map[string]string{"EMAIL_FROM": name + " <sender@example.com>"}),
		jsonReply(200, `{"MessageId":"accepted"}`))
	if err := svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
		t.Fatal(err)
	}
	var got struct{ FromEmailAddress string }
	if err := json.Unmarshal([]byte(stub.bodies[0]), &got); err != nil {
		t.Fatal(err)
	}
	from := got.FromEmailAddress
	if strings.ContainsAny(from, "\r\n") || !isASCII(from) || strings.Count(from, "=?utf-8?b?") < 2 {
		t.Fatalf("FromEmailAddress %q", from)
	}
	encodedName, ok := strings.CutSuffix(from, " <sender@example.com>")
	if !ok {
		t.Fatalf("FromEmailAddress %q", from)
	}
	if decoded, err := new(mime.WordDecoder).DecodeHeader(encodedName); err != nil || decoded != name {
		t.Fatalf("display name %q (%v)", decoded, err)
	}
	// The SMTP header keeps folding the same name.
	if header := svc.t.(*sesTransport).id.headerValue(); !strings.Contains(header, "\r\n ") {
		t.Fatalf("header value not folded: %q", header)
	}
}

func TestSESOptionalFieldsOmitted(t *testing.T) {
	isolateAWS(t)
	svc, stub := sesWithStub(t, sesEnv(nil), jsonReply(200, `{"MessageId":"accepted"}`))
	if err := svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
		t.Fatal(err)
	}
	body := stub.bodies[0]
	if strings.Contains(body, "Html") || strings.Contains(body, "ConfigurationSetName") {
		t.Fatalf("optional fields present: %s", body)
	}
	if stub.reqs[0].Header.Get("X-Amz-Security-Token") != "" {
		t.Fatal("unexpected session token")
	}
}

func TestSESUsesTheSDKCredentialChainWithoutConfiguredKeys(t *testing.T) {
	isolateAWS(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDFROMCHAIN")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "chain-secret")
	t.Setenv("AWS_SESSION_TOKEN", "chain-token")
	env := baseEnv("ses", nil) // no key fields in the configuration map
	svc, stub := sesWithStub(t, env, jsonReply(200, `{"MessageId":"accepted"}`))
	if err := svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
		t.Fatal(err)
	}
	req := stub.reqs[0]
	if !strings.Contains(req.Header.Get("Authorization"), "Credential=AKIDFROMCHAIN/") ||
		req.Header.Get("X-Amz-Security-Token") != "chain-token" {
		t.Fatalf("chain credentials not used: %v", req.Header)
	}
	// The shared credentials file is part of the chain, too.
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	file := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(file, []byte("[default]\naws_access_key_id = AKIDFROMFILE\naws_secret_access_key = file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", file)
	if err := svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stub.reqs[1].Header.Get("Authorization"), "Credential=AKIDFROMFILE/") {
		t.Fatalf("credentials file not used: %v", stub.reqs[1].Header)
	}
}

func TestSESMissingCredentialsFailBeforeAnyRequest(t *testing.T) {
	isolateAWS(t)
	svc, stub := sesWithStub(t, baseEnv("ses", nil), jsonReply(200, `{"MessageId":"accepted"}`))
	assertDelivery(t, svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""))
	if stub.count() != 0 {
		t.Fatalf("unsigned request sent: %d", stub.count())
	}
}

func TestSESIgnoresConfiguredEndpointURLs(t *testing.T) {
	isolateAWS(t)
	t.Setenv("AWS_ENDPOINT_URL", "https://evil.example.com")
	t.Setenv("AWS_ENDPOINT_URL_SESV2", "https://evil-ses.example.com")
	cfg := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfg, []byte("[default]\nendpoint_url = https://evil-profile.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfg)
	svc, stub := sesWithStub(t, sesEnv(map[string]string{"AWS_SES_REGION": "ap-southeast-1"}), jsonReply(200, `{"MessageId":"accepted"}`))
	if err := svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
		t.Fatal(err)
	}
	if host := stub.reqs[0].URL.Host; host != "email.ap-southeast-1.amazonaws.com" {
		t.Fatalf("request went to %q", host)
	}
}

func TestSESRegionFromConfigurationNotAmbientEnvironment(t *testing.T) {
	isolateAWS(t)
	t.Setenv("AWS_REGION", "eu-west-1")
	svc, stub := sesWithStub(t, sesEnv(map[string]string{"AWS_SES_REGION": "us-west-2"}), jsonReply(200, `{"MessageId":"accepted"}`))
	if err := svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
		t.Fatal(err)
	}
	if host := stub.reqs[0].URL.Host; host != "email.us-west-2.amazonaws.com" {
		t.Fatalf("request went to %q", host)
	}
}

func TestSESFailuresAreSanitizedAndNeverRetried(t *testing.T) {
	isolateAWS(t)
	failures := map[string]func(int, *http.Request) (*http.Response, error){
		"rejected": func(int, *http.Request) (*http.Response, error) {
			r := reply(400, "application/json", `{"message":"private body and credentials"}`)
			r.Header.Set("X-Amzn-Errortype", "MessageRejected")
			return r, nil
		},
		"throttled": func(int, *http.Request) (*http.Response, error) {
			r := reply(429, "application/json", `{"message":"slow down"}`)
			r.Header.Set("X-Amzn-Errortype", "TooManyRequestsException")
			return r, nil
		},
		"server error": func(int, *http.Request) (*http.Response, error) {
			return reply(500, "application/json", `{"message":"private"}`), nil
		},
		"unavailable": func(int, *http.Request) (*http.Response, error) {
			return reply(503, "application/json", `{"message":"private"}`), nil
		},
		"transport": func(int, *http.Request) (*http.Response, error) {
			return nil, &timeoutError{"private network failure"}
		},
		"no message id": jsonReply(200, `{}`),
		"empty id":      jsonReply(200, `{"MessageId":""}`),
		"not json":      jsonReply(200, `private`),
	}
	for name, respond := range failures {
		t.Run(name, func(t *testing.T) {
			svc, stub := sesWithStub(t, sesEnv(nil), respond)
			err := svc.Send(context.Background(), "user@example.com", "Subject", "private code", "")
			assertDelivery(t, err)
			if stub.count() != 1 {
				t.Fatalf("calls = %d, want exactly one (no SDK retries)", stub.count())
			}
		})
	}
}

func TestSESTransientErrorsAreRetriedByNobody(t *testing.T) {
	isolateAWS(t)
	// A 500 followed by a success would be hidden by the SDK's default retryer.
	svc, stub := sesWithStub(t, sesEnv(nil), func(call int, _ *http.Request) (*http.Response, error) {
		if call == 1 {
			return reply(500, "application/json", `{}`), nil
		}
		return reply(200, "application/json", `{"MessageId":"second"}`), nil
	})
	assertDelivery(t, svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""))
	if stub.count() != 1 {
		t.Fatalf("calls = %d", stub.count())
	}
}
