#!/bin/bash
# Sets up TLS termination for the CoinApp share API.
#
# The backend itself already runs, bound to 127.0.0.1:8083 so it is not
# reachable from the internet. This puts nginx in front of it on a hostname of
# its own and gives that hostname a certificate, because the web app is served
# over https and a browser refuses to call an http:// API from an https page
# (mixed content).
#
# Needs root: it writes to /etc/nginx and /etc/letsencrypt.
#
# Usage: sudo ./setup-api-host.sh <api-hostname> <email-for-certbot>

set -euo pipefail

HOST="${1:?usage: sudo ./setup-api-host.sh <api-hostname> <email>}"
EMAIL="${2:?usage: sudo ./setup-api-host.sh <api-hostname> <email>}"
BACKEND=127.0.0.1:8083
WEBROOT=/var/www/certbot

if ! getent hosts "$HOST" >/dev/null; then
  echo "error: $HOST does not resolve. Point it at this server first." >&2
  exit 1
fi

mkdir -p "$WEBROOT" /etc/nginx/sites-available

# ---- step 1: plain HTTP vhost, so certbot can validate over the ACME path ----
cat > /etc/nginx/sites-available/coinapp-api.conf <<EOF
server {
    listen 80;
    listen [::]:80;
    server_name $HOST;

    # Leave the ACME path reachable over plain HTTP so the certificate can be
    # issued and renewed; everything else is redirected to TLS below.
    location /.well-known/acme-challenge/ { root $WEBROOT; }
    location / { return 301 https://\$host\$request_uri; }
}
EOF

ln -sf /etc/nginx/sites-available/coinapp-api.conf /etc/nginx/sites-enabled/coinapp-api.conf
nginx -t
systemctl reload nginx

# ---- step 2: issue the certificate ----
if [ ! -d "/etc/letsencrypt/live/$HOST" ]; then
  certbot certonly --webroot -w "$WEBROOT" -d "$HOST" \
    --non-interactive --agree-tos -m "$EMAIL"
fi

# ---- step 3: the real vhost, proxying to the loopback backend ----
cat > /etc/nginx/sites-available/coinapp-api.conf <<EOF
server {
    listen 80;
    listen [::]:80;
    server_name $HOST;

    location /.well-known/acme-challenge/ { root $WEBROOT; }
    location / { return 301 https://\$host\$request_uri; }
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name $HOST;

    ssl_certificate     /etc/letsencrypt/live/$HOST/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/$HOST/privkey.pem;

    # Every request here is a small JSON document; the only body is the share
    # request, which is a uid. Nothing needs a large upload.
    client_max_body_size 16k;

    location = /healthz {
        proxy_pass http://$BACKEND;
        proxy_set_header Host \$host;
        access_log off;
    }

    location /api/ {
        proxy_pass http://$BACKEND;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }

    # This host exists to serve the API. Anything else is a mistake, not a page
    # to render.
    location / { return 404; }
}
EOF

nginx -t
systemctl reload nginx

# Renewal has to reload nginx or the new certificate is not picked up.
grep -q 'webroot -w /var/www/certbot' /etc/letsencrypt/renewal/"$HOST".conf 2>/dev/null || true
if [ -f /etc/letsencrypt/renewal/"$HOST".conf ]; then
  sed -i "s|^authenticator = webroot$|authenticator = webroot\ndeploy_hook = systemctl reload nginx|" \
    /etc/letsencrypt/renewal/"$HOST".conf 2>/dev/null || true
fi

echo
echo "done. verify with:"
echo "  curl -sI https://$HOST/healthz"
echo "  curl -s -X POST -H 'Content-Type: application/json' -d '{}' https://$HOST/api/shares -i | head -1"
