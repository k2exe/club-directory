#!/bin/sh
# Enables :443 only if a cert and key have been manually dropped into
# /etc/nginx/certs (mounted from ./nginx/certs on the host). If they're
# absent — e.g. serving a .local.mesh name with no CA available — nginx
# just runs :80 from default.conf.template, no error, no redirect.
set -eu

CERT=/etc/nginx/certs/fullchain.pem
KEY=/etc/nginx/certs/privkey.pem
SERVER_NAME="${SERVER_NAME:-_}"

if [ -f "$CERT" ] && [ -f "$KEY" ]; then
	echo "25-optional-ssl.sh: cert and key found, enabling HTTPS on :443 for ${SERVER_NAME}"
	SERVER_NAME="$SERVER_NAME" envsubst '${SERVER_NAME}' \
		< /etc/nginx/ssl-templates/ssl.conf.template \
		> /etc/nginx/conf.d/ssl.conf
else
	echo "25-optional-ssl.sh: no cert/key at $CERT / $KEY — serving plain HTTP on :80 only"
fi
