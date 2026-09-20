"""Password-plus-OTP authentication with SQL-backed, single-use challenges."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
import re
import secrets
import smtplib
import ssl
from email.message import EmailMessage

import jwt as pyjwt
import pyotp
import qrcode
import qrcode.image.svg
from argon2.exceptions import InvalidHashError, VerificationError
from flask import jsonify, request
from flask_jwt_extended import (
    get_jwt, get_jwt_identity, get_jwt_request_location, jwt_required, set_access_cookies,
)
from sqlalchemy import delete, insert, select, update

from database import connection, mfa_accounts, mfa_challenges, transaction, users
from i18n import translate as tr


def email_available():
    return bool(os.environ.get("SMTP_HOST") and os.environ.get("SMTP_FROM"))


def send_email_code(address, code):
    """TLS is mandatory; SMTP credentials never enter API responses or SQL."""
    if not email_available():
        raise RuntimeError("SMTP is not configured")
    message = EmailMessage()
    message["From"] = os.environ["SMTP_FROM"]
    message["To"] = address
    message["Subject"] = tr("Your Subtitle Translator verification code")
    message.set_content(tr(
        "Your verification code is {code}. It expires in 5 minutes. If you did not request it, ignore this email.",
        code=code,
    ))
    mode = os.environ.get("SMTP_SECURITY", "starttls").lower()
    if mode not in {"starttls", "ssl"}:
        raise RuntimeError("SMTP_SECURITY must be starttls or ssl")
    port = int(os.environ.get("SMTP_PORT") or ("465" if mode == "ssl" else "587"))
    context = ssl.create_default_context()
    factory = smtplib.SMTP_SSL if mode == "ssl" else smtplib.SMTP
    kwargs = {"context": context} if mode == "ssl" else {}
    with factory(os.environ["SMTP_HOST"], port, timeout=10, **kwargs) as smtp:
        if mode == "starttls":
            smtp.starttls(context=context)
        username = os.environ.get("SMTP_USERNAME", "")
        if username:
            smtp.login(username, os.environ.get("SMTP_PASSWORD", ""))
        smtp.send_message(message)


def mask_email(address):
    local, _, domain = address.partition("@")
    return local[:1] + "***@" + domain if domain else ""


class MFA:
    def __init__(self, app, engine, *, password_hasher, encrypt, decrypt,
                 issue_token, public_user, clock, signing_secret, stable_keys):
        self.engine = engine
        self.password_hasher = password_hasher
        self.encrypt = encrypt
        self.decrypt = decrypt
        self.issue_token = issue_token
        self.public_user = public_user
        self.clock = clock
        self.stable_keys = stable_keys
        # A challenge JWT can never validate as an application access JWT.
        self.key = hmac.digest(signing_secret.encode(), b"subtitle-mfa-v1", "sha256")
        routes = [
            ("/api/auth/mfa", "GET", self.status, True),
            ("/api/auth/mfa/setup", "POST", self.setup, True),
            ("/api/auth/mfa/confirm", "POST", self.confirm, True),
            ("/api/auth/mfa/email", "POST", self.management_email, True),
            ("/api/auth/mfa/disable", "POST", self.disable, True),
            ("/api/auth/mfa/recovery", "POST", self.recovery, True),
            ("/api/auth/mfa/verify", "POST", self.verify_login, False),
            ("/api/auth/mfa/resend", "POST", self.resend, False),
        ]
        for path, method, handler, protected in routes:
            app.add_url_rule(path, "mfa_" + handler.__name__,
                             jwt_required()(handler) if protected else handler,
                             methods=[method])

    def timestamp(self):
        return int(self.clock().timestamp())

    def payload(self):
        if request.content_length and request.content_length > 8192:
            return {}
        value = request.get_json(silent=True)
        return value if isinstance(value, dict) else {}

    def error(self, message=None, status=400, retry=None):
        response = jsonify(error=message or tr("Invalid or expired verification code"))
        if retry:
            response.headers["Retry-After"] = str(max(1, retry))
        return response, status

    def lock_account(self, db, user_id, version):
        # An actual UPDATE serializes writers on SQLite as well as server DBs.
        db.execute(update(users).where(users.c.id == user_id).values(
            token_version=users.c.token_version,
        ))
        user = db.execute(select(users).where(users.c.id == user_id)).first()
        if user is None or not user.active or user.token_version != version:
            return None, None
        account = db.execute(select(mfa_accounts).where(
            mfa_accounts.c.user_id == user_id,
        )).first()
        if account is None:
            db.execute(insert(mfa_accounts).values(user_id=user_id))
            account = db.execute(select(mfa_accounts).where(
                mfa_accounts.c.user_id == user_id,
            )).first()
        return user, account

    def change_account(self, db, user_id, **values):
        db.execute(update(mfa_accounts).where(
            mfa_accounts.c.user_id == user_id,
        ).values(**values))

    def guard(self, user, account):
        if user is None:
            return self.error(tr("Authentication required"), 401)
        remaining = account.locked_until - self.timestamp()
        if remaining > 0:
            return self.error(tr("Too many verification attempts; try again later"), 429, remaining)
        return None

    def failed(self, db, account):
        # Keep this update committed even though the request returns an error.
        failures = account.failures + 1
        locked_until = self.timestamp() + 900 if failures >= 5 else 0
        self.change_account(db, account.user_id,
                            failures=0 if locked_until else failures, locked_until=locked_until)
        if locked_until:
            db.execute(delete(mfa_challenges).where(mfa_challenges.c.user_id == account.user_id))
        return self.error(status=400)

    def password_valid(self, user, password):
        if not isinstance(password, str) or len(password) > 256:
            return False
        try:
            return self.password_hasher.verify(user.password_hash, password)
        except (VerificationError, InvalidHashError):
            return False

    def finish_login(self, user, transport="cookies", **extra):
        token = self.issue_token(user)
        response = jsonify(user=self.public_user(user), **extra,
                           **({"access_token": token} if transport == "header" else {}))
        if transport != "header":
            set_access_cookies(response, token)
        return response

    def status(self):
        with connection(self.engine) as db:
            account = db.execute(select(mfa_accounts).where(
                mfa_accounts.c.user_id == get_jwt_identity(),
            )).first()
        return jsonify(method=account.method if account else "",
                       email=mask_email(account.email) if account else "",
                       recovery_remaining=len(json.loads(account.recovery_hashes)) if account else 0,
                       email_available=email_available(), enrollment_available=self.stable_keys)

    def email_limit(self, db, account):
        timestamp = self.timestamp()
        remaining = account.next_send - timestamp
        if remaining > 0:
            return self.error(tr("Please wait before requesting another email code"), 429, remaining)
        fresh_window = account.send_window + 3600 <= timestamp
        if not fresh_window and account.send_count >= 10:
            return self.error(tr("Please wait before requesting another email code"),
                              429, account.send_window + 3600 - timestamp)
        self.change_account(db, account.user_id, next_send=timestamp + 60,
                            send_window=timestamp if fresh_window else account.send_window,
                            send_count=1 if fresh_window else account.send_count + 1)
        return None

    def digest_code(self, challenge_id, code):
        return hmac.new(self.key, (challenge_id + "\0" + code).encode(), hashlib.sha256).hexdigest()

    def create_challenge(self, db, user, purpose, method, *, email="", secret="", transport="cookies"):
        timestamp = self.timestamp()
        challenge_id = secrets.token_hex(32)
        code = f"{secrets.randbelow(1000000):06d}" if method == "email" else ""
        expires = timestamp + (600 if purpose == "enroll" and method == "totp" else 300)
        db.execute(delete(mfa_challenges).where(mfa_challenges.c.expires <= timestamp))
        # Only the newest challenge of each purpose is usable for an account.
        db.execute(delete(mfa_challenges).where(
            mfa_challenges.c.user_id == user.id, mfa_challenges.c.purpose == purpose,
        ))
        db.execute(insert(mfa_challenges).values(
            id=challenge_id, user_id=user.id, version=user.token_version, purpose=purpose,
            method=method, email=email, secret=secret, expires=expires, transport=transport,
            code_hash=self.digest_code(challenge_id, code) if code else "",
        ))
        token = pyjwt.encode({
            "sub": user.id, "ver": user.token_version, "cid": challenge_id,
            "purpose": purpose, "iat": timestamp, "exp": expires,
            "iss": "subtitle-translator", "aud": "subtitle-mfa",
        }, self.key, algorithm="HS256")
        return {"challenge_token": token, "method": method, "expires_in": expires - timestamp}, code

    def deliver(self, result, email, code):
        if code:
            try:
                send_email_code(email, code)
                result["email_sent"] = True
            except (OSError, smtplib.SMTPException, ValueError, RuntimeError):
                # Do not log SMTP exception bodies: they may contain credentials or mail content.
                result["email_sent"] = False
                result["warning"] = tr("Email could not be sent. Retry later or use a recovery code.")
        return jsonify(result)

    def begin_login(self, original_user, payload):
        transport = "header" if payload.get("token_transport") == "header" else "cookies"
        with transaction(self.engine) as db:
            user, account = self.lock_account(db, original_user.id, original_user.token_version)
            error = self.guard(user, account)
            if error:
                return error
            if user.password_hash != original_user.password_hash:
                return self.error(tr("Authentication required"), 401)
            if not account.method:
                return self.finish_login(user, transport)
            send = account.method == "email" and email_available()
            if send:
                error = self.email_limit(db, account)
                if error:
                    send = False  # Recovery codes remain usable during delivery cooldowns.
            result, code = self.create_challenge(db, user, "login", account.method,
                                                 email=account.email, transport=transport)
            if account.method == "email" and not send:
                code = ""
                result.update(email_sent=False, warning=tr(
                    "Email could not be sent. Retry later or use a recovery code.",
                ))
            result["mfa_required"] = True
        return self.deliver(result, account.email, code)

    def decode_challenge(self, payload, purpose=None):
        token = payload.get("challenge_token")
        if not isinstance(token, str) or len(token) > 2048:
            return None
        try:
            claims = pyjwt.decode(token, self.key, algorithms=["HS256"],
                                  audience="subtitle-mfa", issuer="subtitle-translator",
                                  options={"require": ["sub", "ver", "cid", "purpose", "exp", "iat"]})
        except pyjwt.InvalidTokenError:
            return None
        if purpose and claims["purpose"] != purpose:
            return None
        return claims

    def challenge_row(self, db, claims):
        return db.execute(select(mfa_challenges).where(
            mfa_challenges.c.id == claims["cid"],
            mfa_challenges.c.user_id == claims["sub"],
            mfa_challenges.c.version == claims["ver"],
            mfa_challenges.c.purpose == claims["purpose"],
            mfa_challenges.c.expires > self.timestamp(),
        )).first()

    def totp_step(self, secret, code, last_step=-1):
        if not re.fullmatch(r"[0-9]{6}", code):
            return None
        totp = pyotp.TOTP(self.decrypt(secret))
        current = self.timestamp() // 30
        for step in (current, current - 1, current + 1):
            if step > last_step and hmac.compare_digest(totp.at(step * 30), code):
                return step
        return None

    def check_factor(self, db, account, code, challenge=None):
        if not isinstance(code, str) or len(code) > 64:
            return False
        code = code.strip()
        hashes = json.loads(account.recovery_hashes)
        recovery_hash = hashlib.sha256(code.lower().replace("-", "").encode()).hexdigest()
        if recovery_hash in hashes:
            hashes.remove(recovery_hash)
            self.change_account(db, account.user_id, recovery_hashes=json.dumps(hashes))
            return True
        if account.method == "totp":
            step = self.totp_step(account.secret, code, account.last_step)
            if step is not None:
                self.change_account(db, account.user_id, last_step=step)
                return True
        elif account.method == "email" and challenge is not None and challenge.method == "email":
            return hmac.compare_digest(self.digest_code(challenge.id, code), challenge.code_hash)
        return False

    def verify_login(self):
        payload = self.payload()
        claims = self.decode_challenge(payload, "login")
        if not claims:
            return self.error()
        with transaction(self.engine) as db:
            user, account = self.lock_account(db, claims["sub"], claims["ver"])
            error = self.guard(user, account)
            if error:
                return error
            challenge = self.challenge_row(db, claims)
            if challenge is None or not account.method:
                return self.error()
            if not self.check_factor(db, account, payload.get("code"), challenge):
                return self.failed(db, account)
            db.execute(delete(mfa_challenges).where(mfa_challenges.c.id == challenge.id))
            self.change_account(db, user.id, failures=0, locked_until=0)
            return self.finish_login(user, challenge.transport)

    def setup(self):
        payload = self.payload()
        method = payload.get("method")
        email = payload.get("email", "")
        if method not in ("totp", "email"):
            return self.error(tr("Choose an MFA method"))
        if not self.stable_keys:
            return self.error(tr("Configure JWT_SECRET_KEY before saving secrets"), 503)
        if method == "email":
            if not email_available():
                return self.error(tr("Email verification is unavailable; ask an administrator to configure SMTP"), 503)
            if not isinstance(email, str) or len(email) > 254 or not re.fullmatch(
                r"[a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?\.[a-zA-Z]{2,63}", email,
            ) or len(email.partition("@")[0]) > 64:
                return self.error(tr("Enter a valid email address"))
        else:
            email = ""
        with transaction(self.engine) as db:
            user, account = self.lock_account(db, get_jwt_identity(), get_jwt()["ver"])
            error = self.guard(user, account)
            if error:
                return error
            if not self.password_valid(user, payload.get("password")):
                return self.failed(db, account)
            if account.method:
                return self.error(tr("Disable your current MFA method before replacing it"), 409)
            if method == "email":
                error = self.email_limit(db, account)
                if error:
                    return error
            secret = pyotp.random_base32() if method == "totp" else ""
            result, code = self.create_challenge(db, user, "enroll", method, email=email,
                                                 secret=self.encrypt(secret) if secret else "")
            if secret:
                uri = pyotp.TOTP(secret).provisioning_uri(user.username, issuer_name="Subtitle Translator")
                svg = qrcode.make(uri, image_factory=qrcode.image.svg.SvgPathImage).to_string()
                result.update(secret=secret, provisioning_uri=uri,
                              qr_code="data:image/svg+xml;base64," + base64.b64encode(svg).decode("ascii"))
        return self.deliver(result, email, code)

    def new_recovery_codes(self, db, user_id):
        codes = [secrets.token_hex(12) for _ in range(10)]
        self.change_account(db, user_id, recovery_hashes=json.dumps([
            hashlib.sha256(code.encode()).hexdigest() for code in codes
        ]))
        return ["-".join(code[i:i+6] for i in range(0, 24, 6)) for code in codes]

    def rotate_session(self, db, user, **extra):
        db.execute(delete(mfa_challenges).where(mfa_challenges.c.user_id == user.id))
        db.execute(update(users).where(users.c.id == user.id).values(
            token_version=users.c.token_version + 1,
            updated_at=self.clock().isoformat(timespec="seconds"),
        ))
        refreshed = db.execute(select(users).where(users.c.id == user.id)).first()
        transport = "header" if get_jwt_request_location() == "headers" else "cookies"
        return self.finish_login(refreshed, transport, **extra)

    def confirm(self):
        payload = self.payload()
        claims = self.decode_challenge(payload, "enroll")
        if not claims or claims["sub"] != get_jwt_identity() or claims["ver"] != get_jwt()["ver"]:
            return self.error()
        with transaction(self.engine) as db:
            user, account = self.lock_account(db, claims["sub"], claims["ver"])
            error = self.guard(user, account)
            if error:
                return error
            challenge = self.challenge_row(db, claims)
            if challenge is None or account.method:
                return self.error()
            code = payload.get("code")
            step = None
            valid = False
            if isinstance(code, str) and len(code) <= 64:
                code = code.strip()
                if challenge.method == "totp":
                    step = self.totp_step(challenge.secret, code)
                    valid = step is not None
                else:
                    valid = hmac.compare_digest(self.digest_code(challenge.id, code), challenge.code_hash)
            if not valid:
                return self.failed(db, account)
            self.change_account(db, user.id, method=challenge.method, secret=challenge.secret,
                                email=challenge.email, last_step=step if step is not None else -1,
                                failures=0, locked_until=0)
            codes = self.new_recovery_codes(db, user.id)
            return self.rotate_session(db, user, recovery_codes=codes)

    def management_email(self):
        payload = self.payload()
        with transaction(self.engine) as db:
            user, account = self.lock_account(db, get_jwt_identity(), get_jwt()["ver"])
            error = self.guard(user, account)
            if error:
                return error
            if not self.password_valid(user, payload.get("password")):
                return self.failed(db, account)
            if account.method != "email" or not email_available():
                return self.error(tr("Email verification is unavailable; ask an administrator to configure SMTP"), 503)
            error = self.email_limit(db, account)
            if error:
                return error
            result, code = self.create_challenge(db, user, "manage", "email", email=account.email)
        return self.deliver(result, account.email, code)

    def change_method(self, regenerate=False):
        payload = self.payload()
        with transaction(self.engine) as db:
            user, account = self.lock_account(db, get_jwt_identity(), get_jwt()["ver"])
            error = self.guard(user, account)
            if error:
                return error
            if not account.method:
                return self.error(tr("MFA is not enabled"), 409)
            if not self.password_valid(user, payload.get("password")):
                return self.failed(db, account)
            claims = self.decode_challenge(payload, "manage")
            challenge = None
            if claims and claims["sub"] == user.id and claims["ver"] == user.token_version:
                challenge = self.challenge_row(db, claims)
            if not self.check_factor(db, account, payload.get("code"), challenge):
                return self.failed(db, account)
            self.change_account(db, user.id, failures=0, locked_until=0)
            if regenerate:
                codes = self.new_recovery_codes(db, user.id)
                return self.rotate_session(db, user, recovery_codes=codes)
            self.change_account(db, user.id, method="", email="", secret="", last_step=-1, recovery_hashes="[]")
            return self.rotate_session(db, user)

    def disable(self):
        return self.change_method()

    def recovery(self):
        return self.change_method(regenerate=True)

    def resend(self):
        claims = self.decode_challenge(self.payload())
        if not claims:
            return self.error()
        with transaction(self.engine) as db:
            user, account = self.lock_account(db, claims["sub"], claims["ver"])
            error = self.guard(user, account)
            if error:
                return error
            challenge = self.challenge_row(db, claims)
            if challenge is None or challenge.method != "email":
                return self.error()
            if not email_available():
                return self.error(tr("Email verification is unavailable; ask an administrator to configure SMTP"), 503)
            error = self.email_limit(db, account)
            if error:
                return error
            # Rotation invalidates both the previous JWT challenge and its email code.
            result, code = self.create_challenge(db, user, challenge.purpose, "email",
                                                 email=challenge.email, transport=challenge.transport)
        return self.deliver(result, challenge.email, code)
