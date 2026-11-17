"""
wsgi.py — the gunicorn entry point. Loading this module runs start-up inside
the worker process, so the models are loaded where they are used. If start-up
raises, gunicorn reports the worker failed to boot and the server stops.
"""
import logging

from server import app, startup

try:
    startup()
except Exception:
    logging.getLogger("ekyc").critical("start-up failed; refusing to serve", exc_info=True)
    raise

__all__ = ["app"]
