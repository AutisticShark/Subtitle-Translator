package email

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// resendEndpoint is fixed: configuration never selects the URL.
const resendEndpoint = "https://api.resend.com/emails"

type resendTransport struct {
	id       identity
	key      string
	endpoint string
	client   *http.Client
}

func newResend(env map[string]string, id identity) (transport, error) {
	key, err := required(env, "RESEND_API_KEY")
	if err != nil {
		return nil, err
	}
	return &resendTransport{id: id, key: key, endpoint: resendEndpoint, client: newHTTPClient(nil)}, nil
}

func (r *resendTransport) deliver(ctx context.Context, m message) error {
	payload := map[string]any{"from": r.id.raw, "to": []string{m.to}, "subject": m.subject, "text": m.text}
	if m.html != "" {
		payload["html"] = m.html
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return err
	}
	result, err := postJSON(ctx, r.client, r.endpoint, body.Bytes(), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + r.key,
	})
	if err != nil {
		return err
	}
	if !truthy(result["id"]) || truthy(result["error"]) || truthy(result["statusCode"]) {
		return errRejected
	}
	return nil
}
