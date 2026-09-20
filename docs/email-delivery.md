# Email delivery

Subtitle Translator includes a shared transactional email service in `email_delivery.py`. MFA enrollment, login codes, resends, and account management codes all use this service. Other application features can reuse it without depending on MFA or Flask.

Configuration belongs to the deployment environment, as in the previous SMTP implementation. It is not saved in SQL, Redis, or the administrator settings API. Only email availability is exposed to the account security screen. The translation CLI does not send email.

## Choose a provider

Set `EMAIL_PROVIDER` and `EMAIL_FROM`, then the selected provider's variables. With Compose, edit `.env` and recreate the application service. Install the updated `requirements.txt` for a direct Python deployment. The Dockerfile automatically includes the new root Python module and installs the AWS SDK.

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

SES uses the official Boto3 client for the [SES v2 SendEmail API](https://docs.aws.amazon.com/ses/latest/APIReference-V2/API_SendEmail.html). Verify the sender identity in the selected region and grant the sending identity `ses:SendEmail`. If the account is in the [SES sandbox](https://docs.aws.amazon.com/ses/latest/dg/request-production-access.html), recipients must also be verified, except for the mailbox simulator. `AWS_SES_CONFIGURATION_SET` is optional and must name a configuration set in that region.

Region selection is `AWS_SES_REGION`, then `AWS_REGION`, then `AWS_DEFAULT_REGION`. A region is required. Leave access-key fields blank to use the [AWS SDK credential chain](https://docs.aws.amazon.com/boto3/latest/guide/credentials.html), including IAM roles. Temporary environment credentials also require `AWS_SESSION_TOKEN`. Host credential files and role settings are not automatically mounted or forwarded into Compose containers; supply those deployment-specific mounts or environment settings when using them.

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

Network operations use 10-second connection/socket timeouts with certificate validation. HTTP adapters reject redirects and bound response reads. SES uses the SDK's service endpoint resolution and ignores custom endpoint URL configuration. The service makes one send attempt; it does not automatically retry or switch providers after a failure. A timeout can happen after provider acceptance, so an automatic retry could send duplicate codes. MFA retains its existing SQL send reservations, cooldowns, challenge expiry, and explicit resend flow.

Success indicates acceptance by the provider or SMTP relay; it does not confirm arrival in the inbox. Provider exceptions and response bodies are replaced with a generic error before reaching MFA. Do not enable SMTP or AWS SDK wire/debug logging in a deployment handling real credentials or verification messages.

To validate local configuration without sending mail, run this inside the application's environment:

```console
python -c "from email_delivery import configured_provider; print(type(configured_provider()).__name__)"
```

This checks local configuration only. It does not probe credentials, account permissions, sender verification, quotas, or connectivity. Test delivery through an email MFA enrollment with a mailbox you control. Keep recovery codes available before testing an existing MFA account or changing providers.

The automated suite uses mocked SMTP/HTTP transports, the real AWS SDK with its offline Stubber, and Alibaba's published signature vector. It also checks MFA failures and recovery. These checks do not establish real provider delivery or deployed container operation.

## Add another provider

Implement a factory accepting `(environment, sender)` and returning an object with `send(message: OutboundEmail) -> None`. Validate configuration in the factory without doing network I/O. Raise `EmailConfigurationError` with a fixed, secret-free configuration message when necessary. Submit once in `send`, and raise on rejection or ambiguous failure. The common service sanitizes transport exceptions.

Register trusted code at application startup, before requests are served, in every worker:

```python
from email_delivery import register_provider
from my_mail_adapter import MyMailProvider

register_provider("my_provider", MyMailProvider)
```

Then select `EMAIL_PROVIDER=my_provider`. For an in-tree provider, add its factory to `_PROVIDERS` in `email_delivery.py`. Provider names cannot replace existing registrations, and environment values never cause dynamic imports. Keep mail-provider secrets deployment-only. Add its environment variables to `.env.example` and `compose.yaml`, document setup, and add offline request and failure tests.

Application callers use the same function for all providers:

```python
from email_delivery import send_email

send_email(recipient, "Notification", "Plain text message", html="<p>Message</p>")
```

Callers must enforce their own authorization and send limits before calling this function. Escape user content when building HTML. Keep authentication challenges and their secrets in the MFA layer; mail adapters only deliver already-composed messages.
