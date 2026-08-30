import re
from pathlib import Path


PROJECT_ROOT = Path(__file__).resolve().parents[1]


def test_dockerfile_copies_python_modules_and_localization_catalogs():
    dockerfile = (PROJECT_ROOT / "Dockerfile").read_text("utf-8")

    assert re.search(r"(?m)^COPY \*\.py \./\s*$", dockerfile)
    assert re.search(r"(?m)^COPY locales \./locales\s*$", dockerfile)

