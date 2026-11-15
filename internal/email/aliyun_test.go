package email

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestAliyunSignatureMatchesPublishedProviderVector(t *testing.T) {
	// https://www.alibabacloud.com/help/en/direct-mail/signature
	params := map[string]string{
		"AccessKeyId": "testid", "AccountName": "<a%b'>", "Action": "SingleSendMail",
		"AddressType": "1", "Format": "XML", "HtmlBody": "4", "RegionId": "cn-hangzhou",
		"ReplyToAddress": "true", "SignatureMethod": "HMAC-SHA1",
		"SignatureNonce": "c1b2c332-4cfb-4a0f-b8cc-ebe622aa0a5c", "SignatureVersion": "1.0",
		"Subject": "3", "TagName": "2", "Timestamp": "2016-10-20T06:27:56Z",
		"ToAddress": "1@test.com", "Version": "2015-11-23",
	}
	if got := aliyunSignature(params, "testsecret"); got != "llJfXJjBW3OacrVgxxsITgYaYm0=" {
		t.Fatalf("signature %q", got)
	}
	// A Signature already present is excluded from the canonical string.
	params["Signature"] = "stale"
	if got := aliyunSignature(params, "testsecret"); got != "llJfXJjBW3OacrVgxxsITgYaYm0=" {
		t.Fatalf("signature with stale Signature %q", got)
	}
}

func TestAliyunPercentEncode(t *testing.T) {
	cases := map[string]string{
		"AZaz09-_.~": "AZaz09-_.~", " ": "%20", "+": "%2B", "*": "%2A", "/": "%2F",
		"&=": "%26%3D", "驗": "%E9%A9%97", "~": "~", "<a%b'>": "%3Ca%25b%27%3E",
	}
	for in, want := range cases {
		if got := aliyunPercentEncode(in); got != want {
			t.Errorf("aliyunPercentEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAliyunSignedPostAndRegionalEndpoints(t *testing.T) {
	for region, host := range aliyunEndpoints {
		t.Run(region, func(t *testing.T) {
			svc := mustNew(t, baseEnv("aliyun", map[string]string{
				"ALIYUN_DM_REGION": region, "ALIYUN_SECURITY_TOKEN": "session-token"}))
			tr := svc.t.(*aliyunTransport)
			stub := &stubTransport{respond: jsonReply(200, `{"EnvId":"accepted","RequestId":"request"}`)}
			tr.client = newHTTPClient(stub)
			tr.now = func() time.Time { return time.Date(2026, 11, 15, 12, 30, 5, 0, time.FixedZone("x", 8*3600)) }

			if err := svc.Send(context.Background(), "user@example.com", "驗證 + & ~", "Code 123456", "<p>123456</p>"); err != nil {
				t.Fatal(err)
			}
			req := stub.reqs[0]
			if req.URL.String() != "https://"+host+"/" || req.Method != http.MethodPost || req.URL.RawQuery != "" {
				t.Fatalf("request %s %s", req.Method, req.URL)
			}
			if req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Fatalf("content type %q", req.Header.Get("Content-Type"))
			}
			form, err := url.ParseQuery(stub.bodies[0])
			if err != nil {
				t.Fatal(err)
			}
			params := map[string]string{}
			for key := range form {
				params[key] = form.Get(key)
			}
			want := map[string]string{
				"Action": "SingleSendMail", "Version": "2015-11-23", "Format": "JSON",
				"AccessKeyId": "private-access-id", "RegionId": region,
				"SignatureMethod": "HMAC-SHA1", "SignatureVersion": "1.0",
				"Timestamp":   "2026-11-15T04:30:05Z",
				"AccountName": "sender@example.com", "FromAlias": "Mail", "AddressType": "1", "ReplyToAddress": "false",
				"ToAddress": "user@example.com", "Subject": "驗證 + & ~", "TextBody": "Code 123456",
				"HtmlBody": "<p>123456</p>", "SecurityToken": "session-token",
			}
			for key, value := range want {
				if params[key] != value {
					t.Errorf("%s = %q, want %q", key, params[key], value)
				}
			}
			if len(params) != len(want)+2 { // plus SignatureNonce and Signature
				t.Errorf("unexpected parameters: %v", params)
			}
			if params["Signature"] != aliyunSignature(params, "private-access-secret") {
				t.Error("signature does not verify against the posted parameters")
			}
			// Credentials, recipient and code never appear in the URL.
			if req.URL.RawQuery != "" || req.URL.Path != "/" {
				t.Errorf("URL carries data: %s", req.URL)
			}
			nonce := params["SignatureNonce"]
			if len(nonce) != 32 {
				t.Errorf("nonce %q", nonce)
			}
			if err := svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
				t.Fatal(err)
			}
			second, _ := url.ParseQuery(stub.bodies[1])
			if second.Get("SignatureNonce") == nonce {
				t.Error("nonce was reused")
			}
			if second.Has("HtmlBody") {
				t.Error("HtmlBody sent without HTML")
			}
		})
	}
}

func TestAliyunWithoutOptionalFields(t *testing.T) {
	svc := mustNew(t, baseEnv("aliyun", map[string]string{"EMAIL_FROM": "sender@example.com"}))
	stub := &stubTransport{respond: jsonReply(200, `{"EnvId":"accepted"}`)}
	svc.t.(*aliyunTransport).client = newHTTPClient(stub)
	if err := svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""); err != nil {
		t.Fatal(err)
	}
	form, _ := url.ParseQuery(stub.bodies[0])
	for _, key := range []string{"FromAlias", "HtmlBody", "SecurityToken"} {
		if form.Has(key) {
			t.Errorf("%s must be absent", key)
		}
	}
	if form.Get("RegionId") != "cn-hangzhou" || stub.reqs[0].URL.Host != "dm.aliyuncs.com" {
		t.Errorf("default region wrong: %s %s", form.Get("RegionId"), stub.reqs[0].URL.Host)
	}
}

func TestAliyunRejectionsAreSanitized(t *testing.T) {
	cases := map[string]string{
		"code":        `{"Code":"InvalidKey","Message":"secret"}`,
		"code+envid":  `{"Code":"X","EnvId":"ok"}`,
		"no env id":   `{"RequestId":"ok"}`,
		"empty envid": `{"EnvId":""}`,
		"array":       `[]`,
		"invalid":     `<xml>secret</xml>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			svc := mustNew(t, baseEnv("aliyun", nil))
			stub := &stubTransport{respond: jsonReply(200, body)}
			svc.t.(*aliyunTransport).client = newHTTPClient(stub)
			assertDelivery(t, svc.Send(context.Background(), "user@example.com", "Subject", "private code", ""))
			if stub.count() != 1 {
				t.Fatalf("calls = %d", stub.count())
			}
		})
	}
	for _, status := range []int{307, 400, 403, 429, 500} {
		svc := mustNew(t, baseEnv("aliyun", nil))
		stub := &stubTransport{respond: jsonReply(status, `{"EnvId":"ok"}`)}
		svc.t.(*aliyunTransport).client = newHTTPClient(stub)
		assertDelivery(t, svc.Send(context.Background(), "user@example.com", "Subject", "Text", ""))
		if stub.count() != 1 {
			t.Fatalf("status %d: calls = %d", status, stub.count())
		}
	}
}
