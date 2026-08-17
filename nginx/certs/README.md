# Certs go here

Drop a manually provisioned certificate here to enable HTTPS on :443:

```
nginx/certs/fullchain.pem
nginx/certs/privkey.pem
```

Both files must exist for nginx to enable :443 (checked at container start,
`nginx/docker-entrypoint.d/25-optional-ssl.sh`). If they're absent, nginx
just serves plain HTTP on :80 — no error, no redirect either direction.
Restart the `nginx` service after adding or replacing certs:

```bash
docker compose restart nginx
```
