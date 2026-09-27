#!/usr/bin/env python3
"""Prepare a freshly booted RouterOS guest so mtha can talk to it.

mtha speaks HTTPS only (project.md section 6), but a fresh CHR has www-ssl
disabled, no certificate at all, and an admin account with no password; its
REST API answers on plain HTTP only. slirp forwards this container's port 80
to the guest, so all of it can be done from inside the container at startup.

The certificate is a throwaway self-signed one -- mtha connects with
`insecure_tls`, and this is a lab, so what it certifies does not matter.

Started in the background by testlab/entrypoint.sh and idempotent: it exits
as soon as HTTPS answers, so the cost on a normal restart is one HTTP probe.
"""
import base64
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request

USER = os.environ.get("LAB_ADMIN_USER", "admin")
PASSWORD = os.environ.get("LAB_ADMIN_PASSWORD", "London12")
HTTP = os.environ.get("LAB_GUEST_HTTP", "http://127.0.0.1")
HTTPS = os.environ.get("LAB_GUEST_HTTPS", "https://127.0.0.1")

CTX = ssl._create_unverified_context()


def call(base, path, method="GET", body=None, password=""):
	"""One REST call. Raises on a non-2xx status."""
	data = None if body is None else json.dumps(body).encode()
	req = urllib.request.Request(base + "/rest/" + path, data=data, method=method)
	req.add_header("Content-Type", "application/json")
	token = base64.b64encode(f"{USER}:{password}".encode()).decode()
	req.add_header("Authorization", "Basic " + token)
	with urllib.request.urlopen(req, timeout=20, context=CTX) as resp:
		raw = resp.read()
	return json.loads(raw) if raw else None


def find_id(path, field, value, password):
	for row in call(HTTP, path, password=password):
		if row.get(field) == value:
			return row[".id"]
	raise LookupError(f"{path}: no row with {field}={value}")


def https_ready():
	try:
		call(HTTPS, "system/identity", password=PASSWORD)
		return True
	except Exception:
		return False


def accepted_password():
	"""The password the guest currently takes, or None if it is not up yet.

	slirp accepts TCP before the guest is listening, so a failure here means
	"keep waiting" until something actually answers.
	"""
	for pw in ("", PASSWORD):
		try:
			call(HTTP, "system/identity", password=pw)
			return pw
		except urllib.error.HTTPError as exc:
			if exc.code != 401:
				raise
		except Exception:
			return None
	return None


def main():
	if https_ready():
		print("bootstrap: www-ssl already configured", flush=True)
		return 0

	password = None
	for _ in range(150):
		password = accepted_password()
		if password is not None:
			break
		time.sleep(2)
	if password is None:
		raise TimeoutError("guest never answered on " + HTTP)

	if password == "":
		admin_id = find_id("user", "name", USER, "")
		try:
			call(HTTP, f"user/{admin_id}", "PATCH", {"password": PASSWORD}, password="")
		except urllib.error.HTTPError:
			# RouterOS answers 400 here yet still applies the change (it
			# cannot re-serialise the user once a password is set). The next
			# call authenticates with the new password, so a genuine failure
			# still surfaces as a 401.
			pass
		password = PASSWORD
		print("bootstrap: admin password set", flush=True)

	call(HTTP, "certificate", "PUT", {
		"name": "lab-ca", "common-name": "lab-ca",
		"key-usage": "key-cert-sign,crl-sign"}, password=password)
	call(HTTP, "certificate", "PUT", {
		"name": "lab", "common-name": "lab",
		"key-usage": "tls-server"}, password=password)

	ca_id = find_id("certificate", "name", "lab-ca", password)
	srv_id = find_id("certificate", "name", "lab", password)
	call(HTTP, "certificate/sign", "POST", {".id": ca_id}, password=password)
	call(HTTP, "certificate/sign", "POST", {".id": srv_id, "ca": "lab-ca"},
		password=password)
	print("bootstrap: self-signed certificate created", flush=True)

	ssl_id = find_id("ip/service", "name", "www-ssl", password)
	call(HTTP, f"ip/service/{ssl_id}", "PATCH",
		{"disabled": "false", "certificate": "lab"}, password=password)

	for _ in range(20):
		if https_ready():
			print("bootstrap: www-ssl enabled", flush=True)
			return 0
		time.sleep(2)
	raise TimeoutError("www-ssl did not start answering")


if __name__ == "__main__":
	try:
		sys.exit(main())
	except Exception as exc:  # never take the container down over this
		print(f"bootstrap: FAILED: {exc}", file=sys.stderr, flush=True)
		sys.exit(1)
