"""Server-side CAPTCHA token verification for Turnstile, reCAPTCHA v2, and hCaptcha.

CAPTCHA is a security boundary, not a browser flag: every required token is posted
to the provider's fixed Siteverify endpoint, and any missing configuration or
provider error fails closed. This module holds the verification protocol only; the
caller decides whether an action requires CAPTCHA and turns a failure into a
response.
"""

from __future__ import annotations

import http.client
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
# Transport and decoding failures that mean "no trustworthy answer": fail closed with
# 503. ``http.client.HTTPException`` covers malformed responses such as
# BadStatusLine, IncompleteRead, and LineTooLong; ValueError covers bad JSON/encoding;
# RecursionError covers pathologically nested JSON.
SERVICE_ERRORS = (
    urllib.error.URLError, http.client.HTTPException, TimeoutError, OSError,
    ValueError, RecursionError,
)
_warned_unpinned_hostname = False


def _normalize_hostname(value: str) -> str:
    return value.strip().lower().rstrip(".")


def _warn_unpinned_hostname(provider: str) -> None:
    # Legacy configurations may predate the required hostname. Only the provider's
    # site-key domain allow-list then binds the token to this site; say so once.
    global _warned_unpinned_hostname
    if not _warned_unpinned_hostname:
        _warned_unpinned_hostname = True
        LOGGER.warning(
            "%s CAPTCHA hostname is not configured; skipping the hostname check. "
            "Set captcha_hostname to this site's public hostname.", provider,
        )


def verify_token(
    *, provider: str, action: str, token: Any, site_key: str, secret_key: str,
    configured_hostname: str, remote_addr: str | None,
) -> tuple[str, int] | None:
    """Verify one CAPTCHA token.

    Returns ``None`` when the token is valid, otherwise a translated
    ``(message, http_status)`` pair.

    The expected hostname comes only from the administrator-configured
    ``configured_hostname``; it is never derived from request headers such as
    ``Host`` or ``X-Forwarded-Host``, which the client controls.
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
    except SERVICE_ERRORS as exc:
        LOGGER.warning("%s CAPTCHA verification service error: %s", provider, type(exc).__name__)
        return tr("CAPTCHA is temporarily unavailable"), 503
    if not isinstance(result, dict) or result.get("success") is not True:
        error_codes = result.get("error-codes", []) if isinstance(result, dict) else []
        LOGGER.info("%s CAPTCHA rejected a token: %s", provider, error_codes)
        return failed
    expected_hostname = _normalize_hostname(configured_hostname)
    if expected_hostname:
        actual_hostname = _normalize_hostname(str(result.get("hostname", "")))
        if actual_hostname != expected_hostname:
            LOGGER.warning("%s CAPTCHA returned an unexpected hostname", provider)
            return failed
    else:
        _warn_unpinned_hostname(provider)
    if provider == "turnstile" and result.get("action") != action:
        LOGGER.warning("Turnstile CAPTCHA returned an unexpected action")
        return failed
    return None
