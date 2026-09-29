"""Server-side CAPTCHA token verification for Turnstile, reCAPTCHA v2, and hCaptcha.

CAPTCHA is a security boundary, not a browser flag: every required token is posted
to the provider's fixed Siteverify endpoint, and any missing configuration or
provider error fails closed. This module holds the verification protocol only; the
caller decides whether an action requires CAPTCHA and turns a failure into a
response.
"""

from __future__ import annotations

import json
import logging
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

from i18n import translate as tr


LOGGER = logging.getLogger(__name__)

VERIFY_URLS = {
    "turnstile": "https://challenges.cloudflare.com/turnstile/v0/siteverify",
    "recaptcha": "https://www.google.com/recaptcha/api/siteverify",
    "hcaptcha": "https://api.hcaptcha.com/siteverify",
}
MAX_TOKEN_LENGTH = 8192
TIMEOUT_SECONDS = 8


def _normalize_hostname(value: str) -> str:
    return value.strip().lower().rstrip(".")


def verify_token(
    *, provider: str, action: str, token: Any, site_key: str, secret_key: str,
    configured_hostname: str, request_hostname: str, remote_addr: str | None,
) -> tuple[str, int] | None:
    """Verify one CAPTCHA token.

    Returns ``None`` when the token is valid, otherwise a translated
    ``(message, http_status)`` pair.
    """
    site_key = site_key.strip()
    secret_key = secret_key.strip()
    if not site_key or not secret_key:
        LOGGER.error("CAPTCHA provider %s is active but is missing a site or secret key", provider)
        return tr("CAPTCHA is temporarily unavailable"), 503
    if not isinstance(token, str) or not token.strip():
        return tr("Complete the CAPTCHA challenge"), 400
    token = token.strip()
    failed = (tr("CAPTCHA verification failed; please try again"), 400)
    if len(token) > MAX_TOKEN_LENGTH:
        return failed
    form = {"secret": secret_key, "response": token}
    if remote_addr:
        form["remoteip"] = remote_addr
    if provider == "hcaptcha":
        form["sitekey"] = site_key
    verification_request = urllib.request.Request(
        VERIFY_URLS[provider],
        data=urllib.parse.urlencode(form).encode("ascii"),
        headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "User-Agent": "Subtitle-Translator CAPTCHA verifier",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(verification_request, timeout=TIMEOUT_SECONDS) as response:
            result = json.loads(response.read(65537))
    except (urllib.error.URLError, TimeoutError, OSError, json.JSONDecodeError, ValueError) as exc:
        LOGGER.warning("%s CAPTCHA verification service error: %s", provider, type(exc).__name__)
        return tr("CAPTCHA is temporarily unavailable"), 503
    if not isinstance(result, dict) or result.get("success") is not True:
        error_codes = result.get("error-codes", []) if isinstance(result, dict) else []
        LOGGER.info("%s CAPTCHA rejected a token: %s", provider, error_codes)
        return failed
    expected_hostname = _normalize_hostname(configured_hostname) or _normalize_hostname(request_hostname)
    actual_hostname = _normalize_hostname(str(result.get("hostname", "")))
    if expected_hostname and actual_hostname != expected_hostname:
        LOGGER.warning("%s CAPTCHA returned an unexpected hostname", provider)
        return failed
    if provider == "turnstile" and result.get("action") != action:
        LOGGER.warning("Turnstile CAPTCHA returned an unexpected action")
        return failed
    return None
