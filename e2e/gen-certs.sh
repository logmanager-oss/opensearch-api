#!/bin/sh
# Generates a throwaway CA + node TLS cert for the e2e OpenSearch stack.
#
# Always regenerates from scratch - there is no skip-if-present logic and no
# FORCE flag, so re-running this script always produces a fresh set of certs.
#
# Usage: sh e2e/gen-certs.sh [output-dir]   (default: e2e/certs)
set -eu

OUT_DIR="${1:-e2e/certs}"
mkdir -p "$OUT_DIR"

DAYS=90
SAN_FILE="$OUT_DIR/.san.cnf"
trap 'rm -f "$SAN_FILE"' EXIT

# --- Root CA -----------------------------------------------------------
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$OUT_DIR/ca-key.pem"
openssl req -x509 -new -key "$OUT_DIR/ca-key.pem" -sha256 -days "$DAYS" \
	-subj "/O=osapi-e2e/CN=osapi-e2e-ca" \
	-out "$OUT_DIR/ca.pem"

# --- Node cert, signed by the CA above ----------------------------------
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$OUT_DIR/node-key.pem"
openssl req -new -key "$OUT_DIR/node-key.pem" \
	-subj "/O=osapi-e2e/CN=node" \
	-out "$OUT_DIR/node.csr"

printf 'subjectAltName=DNS:localhost,DNS:opensearch,IP:127.0.0.1\n' >"$SAN_FILE"

openssl x509 -req -in "$OUT_DIR/node.csr" \
	-CA "$OUT_DIR/ca.pem" -CAkey "$OUT_DIR/ca-key.pem" -CAcreateserial \
	-sha256 -days "$DAYS" -extfile "$SAN_FILE" \
	-out "$OUT_DIR/node.pem"
rm -f "$OUT_DIR/node.csr" "$OUT_DIR/ca.srl"

# --- Independent, never-trusted CA for negative TLS tests ---------------
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$OUT_DIR/wrong-ca-key.pem"
openssl req -x509 -new -key "$OUT_DIR/wrong-ca-key.pem" -sha256 -days "$DAYS" \
	-subj "/O=osapi-e2e/CN=osapi-e2e-wrong-ca" \
	-out "$OUT_DIR/wrong-ca.pem"

chmod 644 "$OUT_DIR"/*.pem

echo "Certificates written to $OUT_DIR"
