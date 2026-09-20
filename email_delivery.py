"""Deployment-configured transactional mail adapters, independent of Flask and MFA.

Providers accept one recipient, a subject, plain text, and optional HTML. Register
trusted adapters at application startup; environment values never import code.
"""

from __future__ import annotations

import base64
import hmac
import json
import os
import re
import smtplib
import ssl
import uuid
from contextlib import closing
from dataclasses import dataclass, field
from datetime import datetime, timezone
from email.message import EmailMessage
from email.utils import formatdate, make_msgid, getaddresses
from typing import Callable, Mapping, Protocol
from urllib.parse import quote, urlencode
from urllib.request import HTTPRedirectHandler, HTTPSHandler, Request, build_opener


TIMEOUT = 10
MAX_RESPONSE_BYTES = 64 * 1024
ALIYUN_ENDPOINTS = {
    "cn-hangzhou": "dm.aliyuncs.com",
    "ap-southeast-1": "dm.ap-southeast-1.aliyuncs.com",
    "eu-central-1": "dm.eu-central-1.aliyuncs.com",
    "us-east-1": "dm.us-east-1.aliyuncs.com",
}


class EmailDeliveryError(RuntimeError):
    """Safe to display: never includes provider responses, credentials, or content."""


class EmailConfigurationError(EmailDeliveryError):
    """Missing or invalid deployment configuration."""


@dataclass(frozen=True)
class OutboundEmail:
    to: str = field(repr=False)
    subject: str = field(repr=False)
    text: str = field(repr=False)
    html: str = field(default="", repr=False)


class EmailProvider(Protocol):
    def send(self, message: OutboundEmail) -> None:
        """Send once; raise on rejection or an ambiguous delivery failure."""


def _required(env, name):
    value = env.get(name, "").strip()
    if not value:
        raise EmailConfigurationError(f"{name} is required")
    return value


def _mailbox(value, *, display_name=False):
    # Reject lists and header injection before any provider sees the address.
    if not isinstance(value, str) or not value or any(ord(c) < 32 or ord(c) == 127 for c in value):
        raise EmailConfigurationError("Enter a valid single email address")
    addresses = getaddresses([value])
    if len(addresses) != 1:
        raise EmailConfigurationError("Enter a valid single email address")
    name, address = addresses[0]
    if (not address or len(address) > 254 or len(address.partition("@")[0]) > 64
            or not re.fullmatch(r"[a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?\.[a-zA-Z]{2,63}", address)
            or (not display_name and value != address)):
        raise EmailConfigurationError("Enter a valid single email address")
    return name, address


class SMTPProvider:
    def __init__(self, env, sender):
        self.sender = sender
        self.host = _required(env, "SMTP_HOST")
        self.security = (env.get("SMTP_SECURITY") or "starttls").lower()
        if self.security not in {"starttls", "ssl"}:
            raise EmailConfigurationError("SMTP_SECURITY must be starttls or ssl")
        try:
            self.port = int(env.get("SMTP_PORT") or (465 if self.security == "ssl" else 587))
        except ValueError:
            raise EmailConfigurationError("SMTP_PORT must be between 1 and 65535") from None
        if not 1 <= self.port <= 65535:
            raise EmailConfigurationError("SMTP_PORT must be between 1 and 65535")
        self.username = env.get("SMTP_USERNAME", "")
        self.password = env.get("SMTP_PASSWORD", "")
        if self.password and not self.username:
            raise EmailConfigurationError("SMTP_USERNAME is required with SMTP_PASSWORD")

    def send(self, message):
        mime = EmailMessage()
        mime["From"] = self.sender
        mime["To"] = message.to
        mime["Subject"] = message.subject
        mime["Date"] = formatdate(localtime=False)
        mime["Message-ID"] = make_msgid()
        mime.set_content(message.text)
        if message.html:
            mime.add_alternative(message.html, subtype="html")
        context = ssl.create_default_context()
        factory = smtplib.SMTP_SSL if self.security == "ssl" else smtplib.SMTP
        kwargs = {"context": context} if self.security == "ssl" else {}
        with factory(self.host, self.port, timeout=TIMEOUT, **kwargs) as smtp:
            if self.security == "starttls":
                smtp.starttls(context=context)
            if self.username:
                smtp.login(self.username, self.password)
            if smtp.send_message(mime):
                raise EmailDeliveryError("Email delivery failed")


class SESProvider:
    def __init__(self, env, sender):
        self.sender = sender
        self.region = (env.get("AWS_SES_REGION") or env.get("AWS_REGION")
                       or env.get("AWS_DEFAULT_REGION") or "").strip()
        if not re.fullmatch(r"[a-z]{2}(?:-[a-z]+)+-\d+", self.region):
            raise EmailConfigurationError("AWS_SES_REGION is required and must be a valid region")
        access_key = bool(env.get("AWS_ACCESS_KEY_ID", "").strip())
        secret_key = bool(env.get("AWS_SECRET_ACCESS_KEY", "").strip())
        if access_key != secret_key or (env.get("AWS_SESSION_TOKEN") and not access_key):
            raise EmailConfigurationError("AWS environment credentials require both access key ID and secret access key")
        self.configuration_set = env.get("AWS_SES_CONFIGURATION_SET", "").strip()

    def send(self, message):
        # Use the official SDK for signing, session tokens, and the IAM credential
        # chain. A fresh session avoids sharing mutable default-session state.
        import boto3
        from botocore.config import Config

        body = {"Text": {"Data": message.text, "Charset": "UTF-8"}}
        if message.html:
            body["Html"] = {"Data": message.html, "Charset": "UTF-8"}
        payload = {
            "FromEmailAddress": self.sender,
            "Destination": {"ToAddresses": [message.to]},
            "Content": {"Simple": {
                "Subject": {"Data": message.subject, "Charset": "UTF-8"}, "Body": body,
            }},
        }
        if self.configuration_set:
            payload["ConfigurationSetName"] = self.configuration_set
        config = Config(connect_timeout=TIMEOUT, read_timeout=TIMEOUT,
                        retries={"total_max_attempts": 1}, ignore_configured_endpoint_urls=True)
        with closing(boto3.session.Session().client("sesv2", region_name=self.region, config=config)) as client:
            result = client.send_email(**payload)
        if not isinstance(result, dict) or not result.get("MessageId"):
            raise EmailDeliveryError("Email delivery failed")


class _NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Never forward credentials or verification codes to another endpoint.
        return None


def _post_json(url, data, headers):
    request = Request(url, data=data, headers={"User-Agent": "Subtitle-Translator", **headers}, method="POST")
    opener = build_opener(_NoRedirects(), HTTPSHandler(context=ssl.create_default_context()))
    with opener.open(request, timeout=TIMEOUT) as response:
        if not 200 <= response.status < 300:
            raise EmailDeliveryError("Email delivery failed")
        raw = response.read(MAX_RESPONSE_BYTES + 1)
    if len(raw) > MAX_RESPONSE_BYTES:
        raise EmailDeliveryError("Email delivery failed")
    result = json.loads(raw)
    if not isinstance(result, dict):
        raise EmailDeliveryError("Email delivery failed")
    return result


def aliyun_signature(parameters, secret):
    """DirectMail RPC HMAC-SHA1, as required by the provider's 2015-11-23 API."""
    canonical = "&".join(f"{quote(key, safe='~')}={quote(value, safe='~')}"
                         for key, value in sorted(parameters.items()) if key != "Signature")
    signed = "POST&%2F&" + quote(canonical, safe="~")
    return base64.b64encode(hmac.digest((secret + "&").encode(), signed.encode(), "sha1")).decode()


class AliyunProvider:
    def __init__(self, env, sender):
        self.alias, self.sender = _mailbox(sender, display_name=True)
        if len(self.alias) > 15:
            raise EmailConfigurationError("Aliyun sender display name must be at most 15 characters")
        self.region = (env.get("ALIYUN_DM_REGION") or "cn-hangzhou").strip()
        if self.region not in ALIYUN_ENDPOINTS:
            raise EmailConfigurationError("Unsupported ALIYUN_DM_REGION")
        self.key_id = _required(env, "ALIYUN_ACCESS_KEY_ID")
        self.secret = _required(env, "ALIYUN_ACCESS_KEY_SECRET")
        self.token = env.get("ALIYUN_SECURITY_TOKEN", "")

    def send(self, message):
        params = {
            "Action": "SingleSendMail", "Version": "2015-11-23", "Format": "JSON",
            "AccessKeyId": self.key_id, "RegionId": self.region,
            "SignatureMethod": "HMAC-SHA1", "SignatureVersion": "1.0",
            "SignatureNonce": uuid.uuid4().hex,
            "Timestamp": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "AccountName": self.sender, "AddressType": "1", "ReplyToAddress": "false",
            "ToAddress": message.to, "Subject": message.subject, "TextBody": message.text,
        }
        if self.alias:
            params["FromAlias"] = self.alias
        if message.html:
            params["HtmlBody"] = message.html
        if self.token:
            params["SecurityToken"] = self.token
        params["Signature"] = aliyun_signature(params, self.secret)
        result = _post_json("https://" + ALIYUN_ENDPOINTS[self.region] + "/",
                            urlencode(params).encode(), {"Content-Type": "application/x-www-form-urlencoded"})
        if result.get("Code") or not result.get("EnvId"):
            raise EmailDeliveryError("Email delivery failed")


class ResendProvider:
    def __init__(self, env, sender):
        self.sender = sender
        self.key = _required(env, "RESEND_API_KEY")

    def send(self, message):
        payload = {"from": self.sender, "to": [message.to], "subject": message.subject, "text": message.text}
        if message.html:
            payload["html"] = message.html
        result = _post_json("https://api.resend.com/emails", json.dumps(payload).encode(), {
            "Content-Type": "application/json", "Authorization": "Bearer " + self.key,
        })
        if not result.get("id") or result.get("error") or result.get("statusCode"):
            raise EmailDeliveryError("Email delivery failed")


ProviderFactory = Callable[[Mapping[str, str], str], EmailProvider]
_PROVIDERS: dict[str, ProviderFactory] = {
    "smtp": SMTPProvider, "ses": SESProvider, "aliyun": AliyunProvider, "resend": ResendProvider,
}


def register_provider(name: str, factory: ProviderFactory) -> None:
    """Register a trusted factory once, before serving requests, in every worker."""
    if not re.fullmatch(r"[a-z][a-z0-9_]*", name) or name == "none" or name in _PROVIDERS:
        raise ValueError("Email provider name is invalid or already registered")
    if not callable(factory):
        raise TypeError("Email provider factory must be callable")
    _PROVIDERS[name] = factory


def configured_provider(env: Mapping[str, str] | None = None) -> EmailProvider:
    """Validate configuration without sending mail or probing provider credentials."""
    env = os.environ if env is None else env
    # Empty selection preserves existing SMTP-only deployments. Explicit none
    # disables delivery even when old SMTP credentials remain configured.
    name = env.get("EMAIL_PROVIDER", "").strip().lower() or ("smtp" if env.get("SMTP_HOST") else "none")
    if name == "none":
        raise EmailConfigurationError("Email delivery is disabled")
    if name not in _PROVIDERS:
        raise EmailConfigurationError("Unknown EMAIL_PROVIDER")
    sender = env.get("EMAIL_FROM", "").strip()
    if not sender and name == "smtp":
        sender = env.get("SMTP_FROM", "").strip()
    if not sender:
        raise EmailConfigurationError("EMAIL_FROM is required (SMTP also accepts SMTP_FROM)")
    _mailbox(sender, display_name=True)
    return _PROVIDERS[name](env, sender)


def email_available() -> bool:
    try:
        configured_provider()
        return True
    except EmailConfigurationError:
        return False


def send_email(to: str, subject: str, text: str, *, html: str = "") -> None:
    """Submit a single message once. Success means provider acceptance, not inbox delivery.

    Do not retry or switch providers here: a timeout can occur after acceptance.
    Callers own authorization, send limits, and any explicit resend behavior.
    """
    try:
        _mailbox(to)
        if (not isinstance(subject, str) or not subject.strip() or len(subject) > 256
                or any(ord(c) < 32 or ord(c) == 127 for c in subject)):
            raise EmailConfigurationError("Email subject must be 1 to 256 characters without control characters")
        if not isinstance(text, str) or not text.strip() or not isinstance(html, str):
            raise EmailConfigurationError("Email requires a plain text body and optional HTML string")
        configured_provider().send(OutboundEmail(to, subject, text, html))
    except EmailConfigurationError:
        raise
    except Exception:
        # Provider exceptions may embed secrets or the entire verification email.
        # Hide both their text and chained traceback at this shared boundary.
        raise EmailDeliveryError("Email delivery failed") from None
