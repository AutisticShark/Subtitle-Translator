package email

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 is what the provider's 2015-11-23 RPC signature requires.
	"encoding/base64"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// aliyunEndpoints are the only hosts DirectMail requests are ever sent to.
var aliyunEndpoints = map[string]string{
	"cn-hangzhou":    "dm.aliyuncs.com",
	"ap-southeast-1": "dm.ap-southeast-1.aliyuncs.com",
	"eu-central-1":   "dm.eu-central-1.aliyuncs.com",
	"us-east-1":      "dm.us-east-1.aliyuncs.com",
}

// aliyunPercentEncode is the RPC signature's percent encoding: everything except
// A-Z a-z 0-9 '-' '_' '.' '~' becomes %XX with upper case hex.
func aliyunPercentEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

// aliyunSignature computes the DirectMail RPC HMAC-SHA1 signature for a POST to
// "/": the sorted, percent-encoded parameters (without Signature) are wrapped as
// "POST&%2F&<encoded>" and signed with secret+"&".
func aliyunSignature(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		if key != "Signature" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, key := range keys {
		pairs[i] = aliyunPercentEncode(key) + "=" + aliyunPercentEncode(params[key])
	}
	signed := "POST&%2F&" + aliyunPercentEncode(strings.Join(pairs, "&"))
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(signed))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type aliyunTransport struct {
	id       identity
	region   string
	keyID    string
	secret   string
	token    string
	endpoint string // full URL, fixed per region; tests inside the package may replace it
	client   *http.Client
	now      func() time.Time
}

func newAliyun(env map[string]string, id identity) (transport, error) {
	if utf8.RuneCountInString(id.name) > 15 {
		return nil, configError("Aliyun sender display name must be at most 15 characters")
	}
	region := strings.TrimSpace(env["ALIYUN_DM_REGION"])
	if region == "" {
		region = "cn-hangzhou"
	}
	host, ok := aliyunEndpoints[region]
	if !ok {
		return nil, configError("Unsupported ALIYUN_DM_REGION")
	}
	keyID, err := required(env, "ALIYUN_ACCESS_KEY_ID")
	if err != nil {
		return nil, err
	}
	secret, err := required(env, "ALIYUN_ACCESS_KEY_SECRET")
	if err != nil {
		return nil, err
	}
	return &aliyunTransport{
		id: id, region: region, keyID: keyID, secret: secret, token: env["ALIYUN_SECURITY_TOKEN"],
		endpoint: "https://" + host + "/", client: newHTTPClient(nil), now: time.Now,
	}, nil
}

func (a *aliyunTransport) deliver(ctx context.Context, m message) error {
	params := map[string]string{
		"Action": "SingleSendMail", "Version": "2015-11-23", "Format": "JSON",
		"AccessKeyId": a.keyID, "RegionId": a.region,
		"SignatureMethod": "HMAC-SHA1", "SignatureVersion": "1.0",
		"SignatureNonce": randomHex(16),
		"Timestamp":      a.now().UTC().Format("2006-01-02T15:04:05Z"),
		"AccountName":    a.id.address, "AddressType": "1", "ReplyToAddress": "false",
		"ToAddress": m.to, "Subject": m.subject, "TextBody": m.text,
	}
	if a.id.name != "" {
		params["FromAlias"] = a.id.name
	}
	if m.html != "" {
		params["HtmlBody"] = m.html
	}
	if a.token != "" {
		params["SecurityToken"] = a.token
	}
	params["Signature"] = aliyunSignature(params, a.secret)
	form := url.Values{}
	for key, value := range params {
		form.Set(key, value)
	}
	result, err := postJSON(ctx, a.client, a.endpoint, []byte(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if err != nil {
		return err
	}
	if truthy(result["Code"]) || !truthy(result["EnvId"]) {
		return errRejected
	}
	return nil
}
