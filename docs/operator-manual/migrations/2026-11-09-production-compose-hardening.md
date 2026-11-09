# Migrating: hardened production Docker Compose and default-token refusal

Two breaking changes affect operators who run the published-image stack
(`deployments/docker/docker-compose.yaml`) or any controller whose admin token
is the documented default.

**You are affected if** you started the production compose with its old
defaults, or you run the controller with `UNIFIED_TOKEN` set to the value shown
in the quickstarts and `.env.example`.

## What changed

| Before | After |
|---|---|
| Controller started with any non-empty `UNIFIED_TOKEN`, including the published default. | Controller exits with `the admin token (UNIFIED_TOKEN) is set to the publicly documented default value` unless `UNIFIED_DEV_MODE=1` or `UNIFIED_ALLOW_DEFAULT_TOKEN=1` (`--allow-default-token`) is set. |
| Compose: `UNIFIED_TOKEN` defaulted to the published token. | Compose refuses to start until `UNIFIED_TOKEN` is set (`required variable UNIFIED_TOKEN is missing a value`). |
| Compose: PostgreSQL user and password both `unified`; Garage RPC secret all zeros; Garage S3 key `garageadmin`. | `POSTGRES_PASSWORD`, `GARAGE_RPC_SECRET`, `GARAGE_S3_ACCESS_KEY` and `GARAGE_S3_SECRET_KEY` are required, with no defaults. |
| Compose: `UNIFIED_DEV_MODE` defaulted to `1` (ephemeral key; stored secrets lost on restart). | Dev mode is off. The KEK is read from the file named by `UNIFIED_KEK_FILE` (Compose file secret `kek`, mounted at `/run/secrets/kek`). |
| dind listened on plain TCP 2375 (`DOCKER_TLS_CERTDIR=""`). | dind listens on 2376 with mutual TLS. The agent mounts the `dind-certs` volume read-only and uses `DOCKER_HOST=tcp://dind:2376`, `DOCKER_TLS_VERIFY=1`, `DOCKER_CERT_PATH=/certs/client`. |
| `UNIFIED_AGENT_EXPOSE_ENV=DOCKER_HOST` | `UNIFIED_AGENT_EXPOSE_ENV=DOCKER_HOST,DOCKER_TLS_VERIFY,DOCKER_CERT_PATH` so native `docker` steps keep working. |

The Helm trial Secret and the repo-root development `docker-compose.yaml` set
`UNIFIED_ALLOW_DEFAULT_TOKEN` themselves and keep working unchanged.

## Migrating an existing production compose

1. **Back up first.** Take a Postgres dump and copy your KEK. If you ran with
   the old default `UNIFIED_DEV_MODE=1`, your stored secrets were encrypted with
   an in-memory key that is already gone; re-register them after the upgrade.
   If you already had `UNIFIED_CONTROLLER_KEY_FILE` set, keep using that same
   file as `UNIFIED_KEK_FILE`.
2. Copy `deployments/docker/.env.example` to `.env` and fill in every value
   (the file lists a generator command for each one). For a new KEK run
   `unified-cli keygen --out deployments/docker/kek`.
3. **PostgreSQL password.** The `postgres` image only reads `POSTGRES_PASSWORD`
   when it creates an empty data directory, so an existing `pgdata` volume keeps
   the old password. Change it in place before starting the new stack:

   ```bash
   docker compose -f deployments/docker/docker-compose.yaml up -d postgres
   docker compose -f deployments/docker/docker-compose.yaml exec postgres \
     psql -U unified -c "ALTER USER unified PASSWORD '<the new POSTGRES_PASSWORD>'"
   ```

4. **Garage keys.** Garage creates its default key only on first start. Either
   keep the old S3 key and secret by setting `GARAGE_S3_ACCESS_KEY` and
   `GARAGE_S3_SECRET_KEY` to the old values, or replace the `garagedata` volume
   (this discards archived logs and artifacts). `GARAGE_RPC_SECRET` may be
   changed freely on a single-node Garage.
5. Start the stack: `docker compose --env-file .env -f deployments/docker/docker-compose.yaml up -d`.
   The `dind-storage` volume is reused; `dind-certs` is created on first start.
6. Set `UNIFIED_TOKEN` in every CLI config and CI system that used the old
   default.

Port 8080 is still published on all interfaces over plain HTTP. Put a
TLS-terminating reverse proxy in front, and publish `127.0.0.1:8080:8080` if the
proxy runs on the same host.

## Errors you may see

| Message | Meaning |
|---|---|
| `required variable UNIFIED_TOKEN is missing a value: set UNIFIED_TOKEN in .env ...` | A required variable is unset. Fill it in `.env` (same pattern for `POSTGRES_PASSWORD`, `GARAGE_*`, `UNIFIED_KEK_FILE`). |
| `the admin token (UNIFIED_TOKEN) is set to the publicly documented default value` | Controller refused the default token. Set a random token, or for a throwaway trial set `UNIFIED_DEV_MODE=1` or `UNIFIED_ALLOW_DEFAULT_TOKEN=1`. |
| `password authentication failed for user "unified"` | An old `pgdata` volume still has the previous password; run step 3. |
| `Cannot connect to the Docker daemon at tcp://dind:2376` in agent logs | dind is still generating certificates or the agent does not mount `dind-certs`; check that `docker compose ps` shows dind healthy. |
