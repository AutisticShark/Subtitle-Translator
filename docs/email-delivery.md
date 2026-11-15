# Email delivery

Subtitle Translator includes a shared transactional email service in `internal/email`. MFA enrollment, login codes, resends, and account management codes all use its `Sender` interface. Other application features can reuse it without depending on authentication or HTTP handlers.

Configuration belongs to the deployment environment, as in the previous SMTP implementation. It is not saved in SQL, Redis, or the administrator settings API. Only email availability is exposed to the account security screen.

## Choose a provider

Set `EMAIL_PROVIDER` and `EMAIL_FROM`, then the selected provider's variables. With Compose, edit `.env` and recreate the application service. For a direct deployment, export the variables before starting the Go binary. The AWS SDK is compiled into the server; no additional runtime packages are needed.

`EMAIL_FROM` accepts a single address or a display name and address, such as `Subtitle Translator <noreply@example.com>`. Aliyun restricts the display name to 15 characters; use `Subtitles <noreply@example.com>` or a bare address there.

An unset or blank `EMAIL_PROVIDER` uses SMTP when `SMTP_HOST` is set. Otherwise, mail is disabled. `EMAIL_PROVIDER=none` explicitly disables sending even if old credentials remain configured. An unknown or incomplete provider configuration disables email enrollment and never falls back to another provider. Existing MFA accounts still require a second factor and can use their recovery codes.

### SMTP

```dotenv
EMAIL_PROVIDER=smtp
EMAIL_FROM=Subtitle Translator <noreply@example.com>
SMTP_HOST=smtp.example.com
SMTP_PORT=587
SMTP_SECURITY=starttls
SMTP_USERNAME=your-smtp-user
SMTP_PASSWORD=your-smtp-password
```

Use `SMTP_SECURITY=ssl` with port `465` for implicit TLS. A blank port selects 587 for STARTTLS or 465 for implicit TLS. Certificate and hostname validation are mandatory. Plaintext SMTP is unsupported. Leave both authentication fields blank for a TLS relay that does not require authentication.

Existing deployments can keep `SMTP_FROM`; it is used when `EMAIL_FROM` is blank and the selected provider is SMTP. `SMTP_FROM` is not used by cloud API providers.

### AWS SES

```dotenv
EMAIL_PROVIDER=ses
EMAIL_FROM=Subtitle Translator <noreply@example.com>
AWS_SES_REGION=us-east-1
AWS_SES_CONFIGURATION_SET=
AWS_ACCESS_KEY_ID=your-access-key-id
AWS_SECRET_ACCESS_KEY=your-secret-access-key
AWS_SESSION_TOKEN=
```

SES uses the official AWS SDK for Go v2 for the [SES v2 SendEmail API](https://docs.aws.amazon.com/ses/latest/APIReference-V2/API_SendEmail.html). Verify the sender identity in the selected region and grant the sending identity `ses:SendEmail`. If the account is in the [SES sandbox](https://docs.aws.amazon.com/ses/latest/dg/request-production-access.html), recipients must also be verified, except for the mailbox simulator. `AWS_SES_CONFIGURATION_SET` is optional and must name a configuration set in that region.

Region selection is `AWS_SES_REGION`, then `AWS_REGION`, then `AWS_DEFAULT_REGION`. A region is required. Leave access-key fields blank to use the [AWS SDK credential chain](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html), including IAM roles. Temporary environment credentials also require `AWS_SESSION_TOKEN`. Host credential files and role settings are not automatically mounted or forwarded into Compose containers; supply those deployment-specific mounts or environment settings when using them.

### Aliyun DirectMail

```dotenv
EMAIL_PROVIDER=aliyun
EMAIL_FROM=Subtitles <noreply@example.com>
ALIYUN_DM_REGION=cn-hangzhou
ALIYUN_ACCESS_KEY_ID=your-access-key-id
ALIYUN_ACCESS_KEY_SECRET=your-access-key-secret
ALIYUN_SECURITY_TOKEN=
```

Create and configure the sender address in the chosen region. The adapter uses [SingleSendMail](https://www.alibabacloud.com/help/en/direct-mail/api-dm-2015-11-23-singlesendmail) with `AddressType=1`, `ReplyToAddress=false`, and the provider's [RPC request signature](https://www.alibabacloud.com/help/en/direct-mail/signature). Grant the RAM identity `dm:SingleSendMail`. Temporary credentials can include `ALIYUN_SECURITY_TOKEN`.

Supported [regional endpoints](https://www.alibabacloud.com/help/en/direct-mail/api-dm-2015-11-23-endpoint):

| Region | HTTPS API host |
| --- | --- |
| `cn-hangzhou` (default) | `dm.aliyuncs.com` |
| `ap-southeast-1` | `dm.ap-southeast-1.aliyuncs.com` |
| `eu-central-1` | `dm.eu-central-1.aliyuncs.com` |
| `us-east-1` | `dm.us-east-1.aliyuncs.com` |

The request uses a signed POST body. Credentials, recipient addresses, and verification codes are not placed in the URL. Each request has a fresh nonce and UTC timestamp. Sydney is not offered because its endpoint is decommissioned.

### Resend

```dotenv
EMAIL_PROVIDER=resend
EMAIL_FROM=Subtitle Translator <noreply@example.com>
RESEND_API_KEY=your-sending-api-key
```

Configure a verified sending domain and a sending API key. The adapter calls the [Resend Send Email API](https://resend.com/docs/api-reference/emails/send-email) at `https://api.resend.com/emails` using bearer authentication.

## Delivery behavior and verification

The service supports one recipient, a subject of up to 256 characters, a required plain text body, and optional HTML. It validates addresses and rejects header control characters before calling any provider. Bulk recipients, attachments, custom headers, and bounce processing are outside this interface.

Network operations use bounded connection/socket timeouts with certificate validation and a 30-second overall send deadline. HTTP adapters reject redirects and bound response reads. SES uses the SDK's service endpoint resolution and ignores custom endpoint URL configuration. The service makes one send attempt; it does not automatically retry or switch providers after a failure. A timeout can happen after provider acceptance, so an automatic retry could send duplicate codes. MFA retains its existing SQL send reservations, cooldowns, challenge expiry, and explicit resend flow.

Success indicates acceptance by the provider or SMTP relay; it does not confirm arrival in the inbox. Provider exceptions and response bodies are replaced with a generic error before reaching MFA. Do not enable SMTP or AWS SDK wire/debug logging in a deployment handling real credentials or verification messages.

The server validates local provider configuration at startup without sending mail. The Account screen exposes whether email enrollment is available, not the provider's credentials. Local validation does not probe account permissions, sender verification, quotas, or connectivity. Test delivery through an email enrollment with a mailbox you control. Keep recovery codes available before testing an existing MFA account or changing providers.

The automated suite uses an in-process TLS SMTP server, mocked HTTP transports, the real AWS SDK with offline request checks, and Alibaba's published signature vector. It also checks MFA failures and recovery. These checks do not establish real provider delivery or deployed container operation.

## Add another provider

Implement `email.Sender`:

```go
type Sender interface {
    Send(ctx context.Context, to, subject, text, html string) error
}
```

Validate deployment configuration in the constructor without network I/O. Return a secret-free error wrapping `email.ErrConfiguration` for invalid configuration. Send once; return `email.ErrDelivery` for rejection or an ambiguous failure without including provider response bodies, recipient addresses, or credentials. Honor context cancellation, require verified TLS, reject redirects, and bound response bodies.

For an in-tree provider, add its constructor to the explicit provider switch in `internal/email/email.go`. Environment values must never load code dynamically. Add its variables to `.env.example` and `compose.yaml`, document setup, and add offline request, timeout, redirect, and acceptance tests. Applications embedding this server can also inject a trusted `Sender` through `app.Config.Email`.

Application callers can construct a sender with `email.FromEnv()` or `email.New(environment)`. A disabled provider returns `(nil, nil)`, so check for nil before calling `sender.Send(ctx, recipient, subject, text, html)`.

Callers enforce authorization and send limits before invoking the transport. Escape user content when building HTML. Keep challenges and secrets in the MFA layer; mail adapters deliver already-composed messages.
