"""Experimental EntroQ APIs.

Anything under ``entroq.experimental`` has NO stability guarantee: it may change
incompatibly or be removed in any release, including patch releases. Pin an exact
version if you depend on it.

Currently home to ``entroq.experimental.pg``, the direct-to-PostgreSQL client. It
is a full backend implementation in Python (it calls the same stored procedures
the Go ``eqpg`` backend does), which makes it a maintenance-sensitive surface, so
it lives here rather than in the stable client. The stable, supported path is the
thin ``EntroQJSON`` client talking to a Go server.
"""
