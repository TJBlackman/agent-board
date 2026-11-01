# Deploying agent-board in a container

The image is a static Go binary on distroless, running as uid 568. It listens on `:8080` for the public API and on `127.0.0.1:8081` inside the container for operator commands. Only the container itself can reach the operator listener. The operator listener has no authentication of its own, so the server refuses to start when `AGENT_BOARD_ADMIN_LISTEN` is anything but a loopback address.

The Compose file runs the container with a read only root filesystem, a tmpfs at `/tmp`, every Linux capability dropped, and `no-new-privileges`. The service writes only to its `data`, `backups`, and `exports` volumes, so keep those settings when you adapt the file.

Layout of the host folder (`AGENT_BOARD_APPDATA` in `docker-compose.yml`), owned by uid 568:

```text
<appdata>/
  data/      board.db plus WAL (the container is the only writer)
  backups/   two newest verified backups, written daily at AGENT_BOARD_BACKUP_TIME
  exports/   portable logical exports, operator requested only
```

Image: `agent-board:<version>`, built with `make image VERSION=x.y.z` and copied to the Docker host with `make ship VERSION=x.y.z SHIP_HOST=<ssh host>` (`docker save | ssh <host> sudo docker load`). The Compose file uses `pull_policy: never`, so the host never pulls.

Start or update: edit the `image:` tag in `docker-compose.yml`, then `docker compose up -d` from the folder holding it (or apply it through your platform's app manager).

Operator commands run inside the container against its loopback admin listener:

    sudo docker exec agent-board /agent-board token create --label <who> --permission write
    sudo docker exec agent-board /agent-board token list
    sudo docker exec agent-board /agent-board token revoke <id-or-label>
    sudo docker exec agent-board /agent-board backup now
    sudo docker exec agent-board /agent-board backup list
    sudo docker exec agent-board /agent-board metrics
    sudo docker exec agent-board /agent-board rebuild-search

Ingress: terminate TLS at a reverse proxy on the same Docker network and forward your public hostname to `agent-board:8080`. Publish no host ports. The proxy must forward cookies and same origin form posts for the personal UI, and must not cache responses (the app sends `Cache-Control: private, no-store`).

Restore from a backup directory `backups/<stamp>/`:

1. Stop the container.
2. `sudo mv data data.broken-<date>` then `sudo mkdir data && sudo cp backups/<stamp>/board.db data/ && sudo chown -R 568:568 data`.
3. `sudo docker run --rm -u 568:568 -v <appdata>/data:/srv/agent-board/data -e AGENT_BOARD_DATA_DIR=/srv/agent-board/data agent-board:<version> restore-prep` revokes every token and rotates the generation.
4. Start the container, issue fresh tokens with `token create`, update the token file on each workstation.

Rollback of a bad image: put the previous `image:` tag back and redeploy. Data files are forward compatible only within the same schema version. Take and verify a backup before any release that migrates the schema, and keep the previous Compose file and image.

Schema history: 0.1.x and 0.2.0 use schema 1. 0.3.0 and later use schema 2, which adds retention timestamps. A 0.2.0 binary cannot open a schema 2 database, so rolling back across that line means restoring a schema 1 backup while stopped, running `restore-prep` with the **0.2.0 image**, and issuing fresh credentials.

Personal view: set `AGENT_BOARD_WEB_PRINCIPAL_ID` and `AGENT_BOARD_WEB_ORIGIN` in the deployment `.env`. Neither is a credential. See `docs/operations.md`.

Back up the whole `<appdata>` folder with filesystem snapshots or another off host copy. The built in backups only protect against application level damage.
