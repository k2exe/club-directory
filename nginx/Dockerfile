FROM nginx:1.27-alpine

# default.conf.template is picked up automatically by the base image's
# docker-entrypoint.d/20-envsubst-on-templates.sh and always rendered to
# conf.d/default.conf — plain HTTP on :80, no redirect, no TLS requirement.
COPY templates/default.conf.template /etc/nginx/templates/default.conf.template

# ssl.conf.template lives outside /etc/nginx/templates so it is NOT rendered
# automatically. 25-optional-ssl.sh renders it to conf.d/ssl.conf itself, but
# only if a cert and key are present under /etc/nginx/certs — so HTTPS is
# opt-in, and plain :80 is unaffected either way.
COPY ssl-templates/ssl.conf.template /etc/nginx/ssl-templates/ssl.conf.template
COPY docker-entrypoint.d/25-optional-ssl.sh /docker-entrypoint.d/25-optional-ssl.sh
RUN chmod +x /docker-entrypoint.d/25-optional-ssl.sh
