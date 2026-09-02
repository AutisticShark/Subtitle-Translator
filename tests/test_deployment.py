import ast
import re
from pathlib import Path


PROJECT_ROOT = Path(__file__).resolve().parents[1]


def test_dockerfile_copies_python_modules_and_localization_catalogs():
    dockerfile = (PROJECT_ROOT / "Dockerfile").read_text("utf-8")

    assert re.search(r"(?m)^COPY \*\.py \./\s*$", dockerfile)
    assert re.search(r"(?m)^COPY locales \./locales\s*$", dockerfile)


def test_direct_flask_server_binds_to_loopback_only():
    tree = ast.parse((PROJECT_ROOT / "webapp.py").read_text("utf-8"))
    run_calls = [
        node for node in ast.walk(tree)
        if isinstance(node, ast.Call)
        and isinstance(node.func, ast.Attribute)
        and isinstance(node.func.value, ast.Name)
        and node.func.value.id == "app"
        and node.func.attr == "run"
    ]

    assert len(run_calls) == 1
    host = next(
        keyword.value for keyword in run_calls[0].keywords if keyword.arg == "host"
    )
    assert isinstance(host, ast.Constant)
    assert host.value == "127.0.0.1"
