"""Offline wire-contract and failure tests; never send real mail."""

import io
import json
import smtplib
import ssl
from pathlib import Path
from unittest.mock import MagicMock, Mock
from urllib.error import HTTPError
from urllib.parse import parse_qs

import boto3
import pytest
from botocore.stub import Stubber

import email_delivery as mail


def configure(monkeypatch, provider="resend", **extra):
    values = {
        "EMAIL_PROVIDER": provider, "EMAIL_FROM": "Mail <sender@example.com>",
        "SMTP_HOST": "smtp.example.com", "RESEND_API_KEY": "private-resend-key",
        "AWS_SES_REGION": "us-east-1", "ALIYUN_ACCESS_KEY_ID": "private-access-id",
        "ALIYUN_ACCESS_KEY_SECRET": "private-access-secret",
        **extra,
    }
    for key, value in values.items():
        monkeypatch.setenv(key, value)


def capture_http(monkeypatch, body=b'{"id":"accepted"}', status=200):
    response = MagicMock()
    response.__enter__.return_value = response
    response.status = status
    response.read.return_value = body
    opener = Mock()
    opener.open.return_value = response
    factory = Mock(return_value=opener)
    monkeypatch.setattr(mail, "build_opener", factory)
    return factory, opener, response


def test_legacy_smtp_and_explicit_disable(monkeypatch):
    assert not mail.email_available()
    monkeypatch.setenv("SMTP_HOST", "smtp.example.com")
    monkeypatch.setenv("SMTP_FROM", "Original <sender@example.com>")
    provider = mail.configured_provider()
    assert isinstance(provider, mail.SMTPProvider)
    assert provider.sender == "Original <sender@example.com>"
    monkeypatch.setenv("EMAIL_FROM", "New <new@example.com>")
    assert mail.configured_provider().sender == "New <new@example.com>"
    monkeypatch.setenv("EMAIL_PROVIDER", "none")
    assert not mail.email_available()
    with pytest.raises(mail.EmailConfigurationError, match="disabled"):
        mail.send_email("user@example.com", "Subject", "Text")


@pytest.mark.parametrize("changes", [
    {"EMAIL_PROVIDER": "typo"}, {"EMAIL_FROM": ""},
    {"EMAIL_FROM": "a@example.com,b@example.com"},
    {"EMAIL_FROM": "sender@example.com\r\nBcc: extra@example.com"},
    {"EMAIL_PROVIDER": "smtp", "SMTP_SECURITY": "none"},
    {"EMAIL_PROVIDER": "smtp", "SMTP_PORT": "invalid"},
    {"EMAIL_PROVIDER": "smtp", "SMTP_PORT": "0"},
    {"EMAIL_PROVIDER": "smtp", "SMTP_PORT": "65536"},
    {"EMAIL_PROVIDER": "smtp", "SMTP_HOST": ""},
    {"EMAIL_PROVIDER": "smtp", "SMTP_PASSWORD": "secret", "SMTP_USERNAME": ""},
    {"EMAIL_PROVIDER": "ses", "AWS_SES_REGION": ""},
    {"EMAIL_PROVIDER": "ses", "AWS_SES_REGION": "https://evil.example"},
    {"EMAIL_PROVIDER": "ses", "AWS_ACCESS_KEY_ID": "incomplete"},
    {"EMAIL_PROVIDER": "ses", "AWS_SESSION_TOKEN": "incomplete"},
    {"EMAIL_PROVIDER": "aliyun", "ALIYUN_DM_REGION": "evil.example"},
    {"EMAIL_PROVIDER": "aliyun", "EMAIL_FROM": "SixteenCharAlias <sender@example.com>!"},
    {"EMAIL_PROVIDER": "aliyun", "EMAIL_FROM": "Subtitle Translator <sender@example.com>"},
    {"EMAIL_PROVIDER": "aliyun", "ALIYUN_ACCESS_KEY_SECRET": ""},
    {"RESEND_API_KEY": ""},
])
def test_invalid_configuration_fails_closed_without_smtp_fallback(monkeypatch, changes):
    configure(monkeypatch, **changes)
    assert not mail.email_available()
    with pytest.raises(mail.EmailConfigurationError):
        mail.configured_provider()


@pytest.mark.parametrize("mode,port", [("starttls", 587), ("ssl", 465)])
def test_smtp_tls_authentication_and_multipart(monkeypatch, mode, port):
    configure(monkeypatch, "smtp", SMTP_SECURITY=mode, SMTP_USERNAME="mailer", SMTP_PASSWORD="secret")
    smtp = MagicMock()
    smtp.__enter__.return_value = smtp
    smtp.send_message.return_value = {}
    factory = Mock(return_value=smtp)
    monkeypatch.setattr(mail.smtplib, "SMTP" if mode == "starttls" else "SMTP_SSL", factory)
    mail.send_email("user@example.com", "Verification 驗證", "plain text", html="<p>html</p>")
    assert factory.call_args.args == ("smtp.example.com", port)
    assert factory.call_args.kwargs["timeout"] == 10
    if mode == "starttls":
        context = smtp.starttls.call_args.kwargs["context"]
        assert [call[0] for call in smtp.method_calls] == ["starttls", "login", "send_message"]
    else:
        context = factory.call_args.kwargs["context"]
        smtp.starttls.assert_not_called()
    assert context.verify_mode == ssl.CERT_REQUIRED and context.check_hostname
    smtp.login.assert_called_once_with("mailer", "secret")
    message = smtp.send_message.call_args.args[0]
    assert str(message["From"]) == "Mail <sender@example.com>"
    assert message["To"] == "user@example.com"
    assert message["Subject"] == "Verification 驗證"
    assert message.get_body("plain").get_content().strip() == "plain text"
    assert message.get_body("html").get_content().strip() == "<p>html</p>"


def test_tls_failure_never_authenticates_or_sends(monkeypatch):
    configure(monkeypatch, "smtp", SMTP_USERNAME="mailer", SMTP_PASSWORD="secret")
    smtp = MagicMock()
    smtp.__enter__.return_value = smtp
    smtp.starttls.side_effect = smtplib.SMTPNotSupportedError("private response")
    factory = Mock(return_value=smtp)
    monkeypatch.setattr(mail.smtplib, "SMTP", factory)
    with pytest.raises(mail.EmailDeliveryError, match="^Email delivery failed$"):
        mail.send_email("user@example.com", "Subject", "private code")
    smtp.login.assert_not_called()
    smtp.send_message.assert_not_called()
    assert factory.call_count == 1


def test_ses_sdk_contract_and_no_automatic_retries(monkeypatch):
    configure(monkeypatch, "ses", AWS_SES_CONFIGURATION_SET="transactional")
    session = boto3.session.Session(aws_access_key_id="testing", aws_secret_access_key="testing")
    client = session.client("sesv2", region_name="us-east-1")
    expected = {
        "FromEmailAddress": "Mail <sender@example.com>",
        "Destination": {"ToAddresses": ["user@example.com"]},
        "Content": {"Simple": {"Subject": {"Data": "Subject", "Charset": "UTF-8"}, "Body": {
            "Text": {"Data": "Text", "Charset": "UTF-8"},
            "Html": {"Data": "<p>HTML</p>", "Charset": "UTF-8"},
        }}}, "ConfigurationSetName": "transactional",
    }
    stubber = Stubber(client)
    stubber.add_response("send_email", {"MessageId": "accepted"}, expected)
    stubber.add_client_error("send_email", service_error_code="MessageRejected",
                             service_message="private body and credentials", expected_params=expected)
    factory = Mock(return_value=client)
    monkeypatch.setattr(boto3.session, "Session", lambda: Mock(client=factory))
    with stubber:
        mail.send_email("user@example.com", "Subject", "Text", html="<p>HTML</p>")
        config = factory.call_args.kwargs["config"]
        assert config.connect_timeout == config.read_timeout == 10
        assert config.retries == {"total_max_attempts": 1}
        assert config.ignore_configured_endpoint_urls is True
        assert factory.call_args.args == ("sesv2",)
        assert factory.call_args.kwargs["region_name"] == "us-east-1"
        with pytest.raises(mail.EmailDeliveryError, match="^Email delivery failed$"):
            mail.send_email("user@example.com", "Subject", "Text", html="<p>HTML</p>")
        stubber.assert_no_pending_responses()


def test_aliyun_signature_matches_published_provider_vector():
    # https://www.alibabacloud.com/help/en/direct-mail/signature
    parameters = {
        "AccessKeyId": "testid", "AccountName": "<a%b'>", "Action": "SingleSendMail",
        "AddressType": "1", "Format": "XML", "HtmlBody": "4", "RegionId": "cn-hangzhou",
        "ReplyToAddress": "true", "SignatureMethod": "HMAC-SHA1",
        "SignatureNonce": "c1b2c332-4cfb-4a0f-b8cc-ebe622aa0a5c", "SignatureVersion": "1.0",
        "Subject": "3", "TagName": "2", "Timestamp": "2016-10-20T06:27:56Z",
        "ToAddress": "1@test.com", "Version": "2015-11-23",
    }
    assert mail.aliyun_signature(parameters, "testsecret") == "llJfXJjBW3OacrVgxxsITgYaYm0="


@pytest.mark.parametrize("region,host", list(mail.ALIYUN_ENDPOINTS.items()))
def test_aliyun_signed_post_and_regional_sender(monkeypatch, region, host):
    configure(monkeypatch, "aliyun", ALIYUN_DM_REGION=region, ALIYUN_SECURITY_TOKEN="session-token")
    _, opener, _ = capture_http(monkeypatch, b'{"EnvId":"accepted", "RequestId":"request"}')
    mail.send_email("user@example.com", "驗證 + & ~", "Code 123456", html="<p>123456</p>")
    request = opener.open.call_args.args[0]
    assert request.full_url == "https://" + host + "/"
    assert request.method == "POST" and "?" not in request.full_url
    params = {key: value[0] for key, value in parse_qs(request.data.decode()).items()}
    assert params["AccountName"] == "sender@example.com"
    assert params["FromAlias"] == "Mail"
    assert params["AddressType"] == "1" and params["ReplyToAddress"] == "false"
    assert params["Subject"] == "驗證 + & ~"
    assert params["TextBody"] == "Code 123456" and params["HtmlBody"] == "<p>123456</p>"
    assert params["SecurityToken"] == "session-token"
    assert params["Signature"] == mail.aliyun_signature(params, "private-access-secret")
    first_nonce = params["SignatureNonce"]
    mail.send_email("user@example.com", "Subject", "Text")
    assert parse_qs(opener.open.call_args.args[0].data.decode())["SignatureNonce"][0] != first_nonce


def test_resend_wire_payload_and_redirect_policy(monkeypatch):
    configure(monkeypatch)
    factory, opener, response = capture_http(monkeypatch)
    mail.send_email("user@example.com", "Subject", "Text", html="<p>HTML</p>")
    request = opener.open.call_args.args[0]
    assert request.full_url == "https://api.resend.com/emails"
    assert request.get_header("Authorization") == "Bearer private-resend-key"
    assert json.loads(request.data) == {
        "from": "Mail <sender@example.com>", "to": ["user@example.com"],
        "subject": "Subject", "text": "Text", "html": "<p>HTML</p>",
    }
    assert opener.open.call_args.kwargs["timeout"] == 10
    response.read.assert_called_once_with(mail.MAX_RESPONSE_BYTES + 1)
    redirect = factory.call_args.args[0]
    for code in (301, 302, 303, 307, 308):
        assert redirect.redirect_request(request, None, code, "", {}, "https://other.example") is None


@pytest.mark.parametrize("provider,body,status", [
    ("resend", b'{"id":"ok", "error":"secret"}', 200),
    ("resend", b'{"message":"secret"}', 200), ("resend", b'[]', 200),
    ("resend", b'not json: private', 200), ("resend", b'{"id":"ok"}', 429),
    ("resend", b'x' * (mail.MAX_RESPONSE_BYTES + 1), 200),
    ("aliyun", b'{"Code":"InvalidKey", "Message":"secret"}', 200),
    ("aliyun", b'{"RequestId":"ok"}', 200),
], ids=["resend-error", "missing-id", "array", "invalid-json", "http-error", "oversized", "aliyun-error", "missing-env-id"])
def test_provider_rejection_and_malformed_responses_are_sanitized(monkeypatch, provider, body, status):
    configure(monkeypatch, provider)
    _, opener, _ = capture_http(monkeypatch, body, status)
    with pytest.raises(mail.EmailDeliveryError, match="^Email delivery failed$") as error:
        mail.send_email("user@example.com", "Subject", "private code")
    assert error.value.__suppress_context__
    assert opener.open.call_count == 1


@pytest.mark.parametrize("error", [TimeoutError("private code"), HTTPError(
    "https://api.resend.com/emails", 403, "private key", {}, io.BytesIO(b"secret")),
])
def test_network_errors_are_sanitized_without_retry(monkeypatch, error):
    configure(monkeypatch)
    _, opener, _ = capture_http(monkeypatch)
    opener.open.side_effect = error
    with pytest.raises(mail.EmailDeliveryError, match="^Email delivery failed$"):
        mail.send_email("user@example.com", "Subject", "Text")
    assert opener.open.call_count == 1


@pytest.mark.parametrize("recipient,subject,text", [
    ("a@example.com,b@example.com", "Subject", "Text"),
    ("a@example.com\r\nBcc: b@example.com", "Subject", "Text"),
    ("user@example.com", "Subject\nBcc: a@example.com", "Text"),
    ("user@example.com", "", "Text"), ("user@example.com", "x" * 257, "Text"),
    ("user@example.com", "Subject", ""),
])
def test_invalid_message_never_reaches_transport(monkeypatch, recipient, subject, text):
    configure(monkeypatch)
    transport = Mock()
    monkeypatch.setattr(mail, "_post_json", transport)
    with pytest.raises(mail.EmailConfigurationError):
        mail.send_email(recipient, subject, text)
    transport.assert_not_called()


def test_extension_registration_and_message_privacy(monkeypatch):
    monkeypatch.setattr(mail, "_PROVIDERS", dict(mail._PROVIDERS))
    provider = Mock()
    factory = Mock(return_value=provider)
    mail.register_provider("custom", factory)
    configure(monkeypatch, "custom")
    assert mail.email_available()
    mail.send_email("user@example.com", "Subject", "private OTP")
    message = provider.send.call_args.args[0]
    assert message.to == "user@example.com" and message.text == "private OTP"
    assert "private" not in repr(message) and "user@example.com" not in repr(message)
    for name in ("custom", "smtp", "none", "module.Class"):
        with pytest.raises(ValueError):
            mail.register_provider(name, factory)


def test_compose_forwards_email_configuration():
    root = Path(__file__).resolve().parents[1]
    example = (root / ".env.example").read_text("utf-8")
    compose = (root / "compose.yaml").read_text("utf-8")
    for line in example.splitlines():
        if line.startswith(("EMAIL_", "SMTP_", "AWS_", "ALIYUN_", "RESEND_")):
            name = line.partition("=")[0]
            assert f"      {name}: \"${{{name}:" in compose
    assert 'SMTP_PORT: "${SMTP_PORT:-}"' in compose
