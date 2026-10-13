"""Keep offline email tests independent of deployment credentials."""

import os
import sys

import pytest


@pytest.fixture(autouse=True)
def isolated_email_environment(monkeypatch):
    for name in tuple(os.environ):
        if name.startswith(("EMAIL_", "SMTP_", "AWS_", "ALIYUN_", "RESEND_")):
            monkeypatch.delenv(name)
    monkeypatch.setenv("AWS_EC2_METADATA_DISABLED", "true")


@pytest.fixture(autouse=True)
def restore_flask_config():
    """Undo per-test ``app.config`` changes (DEBUG enables Echo, CSRF settings, ...).

    Without this, a module that enables debug mode or disables CSRF leaks that
    into every later module, so results depend on test order.
    """
    webapp = sys.modules.get("webapp")
    saved = dict(webapp.app.config) if webapp else None
    yield
    if webapp:
        webapp.app.config.clear()
        webapp.app.config.update(saved)
