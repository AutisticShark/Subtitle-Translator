"""Gunicorn hooks for the production container.

Gunicorn reads ``./gunicorn.conf.py`` automatically; the Dockerfile also names
it explicitly. Listener, worker, thread, and timeout options stay on the
command line in the Dockerfile.

Database preparation and interrupted-job recovery run exactly once, in the
master process, before any worker is forked. Recovery marks every queued,
processing, or canceling job as finished, which is correct only while no
worker can be running one. Without this hook each worker would recover at
import time, and a worker that boots or restarts later would fail jobs that
its sibling workers are still translating.
"""

import os
import sys
from pathlib import Path

BASE_DIR = Path(__file__).resolve().parent
if str(BASE_DIR) not in sys.path:
    sys.path.insert(0, str(BASE_DIR))


def on_starting(server):
    from database import initialize_before_workers

    # Keep this resolution identical to webapp.DATA_DIR / webapp.DB_PATH.
    data_dir = Path(os.environ.get("DATA_DIR", BASE_DIR / "data")).resolve()
    (data_dir / "jobs").mkdir(parents=True, exist_ok=True)
    initialize_before_workers(data_dir / "app.db")
    server.log.info("Database prepared and interrupted jobs recovered before forking workers")
