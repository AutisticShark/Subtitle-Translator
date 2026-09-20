"""Keep offline email tests independent of deployment credentials."""

import os

import pytest


@pytest.fixture(autouse=True)
def isolated_email_environment(monkeypatch):
    for name in tuple(os.environ):
        if name.startswith(("EMAIL_", "SMTP_", "AWS_", "ALIYUN_", "RESEND_")):
            monkeypatch.delenv(name)
    monkeypatch.setenv("AWS_EC2_METADATA_DISABLED", "true")
