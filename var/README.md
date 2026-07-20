# Runtime data

`scripts/cloudctl.sh init` creates the following ignored runtime files here:

- `pki/`: private CA, Nginx server identity, and internal-client identity.
- `secrets/fetch_auth_token`: application-level Bearer token.
- `log/nginx/`: Nginx access and error logs mapped from the container.
- `log/fetch-worker/worker.log`: Worker JSON application log mapped from the container.

Back up this directory securely. Never commit its generated contents or include `ca.key`/`server.key` in the client bundle.
