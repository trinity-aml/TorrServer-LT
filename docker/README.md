## TorrServer-LT — Docker

The published image bakes the cross-built `TorrServer-LT` binary (no download at
runtime) and is multi-arch (`linux/amd64`, `linux/arm64`, `linux/arm/v7`).

Source code: https://github.com/trinity-aml/TorrServer-LT

Image (GHCR): `ghcr.io/trinity-aml/torrserver-lt`

```bash
docker run --rm -d --name torrserver -v ~/ts:/opt/ts -p 8090:8090 ghcr.io/trinity-aml/torrserver-lt:latest
```

Then open <http://127.0.0.1:8090>.

Full Docker / Docker Compose usage and the complete environment-variable list
(`TS_PORT`, `TS_CONF_PATH`, `TS_TORR_DIR`, `TS_LOG_PATH`, `TS_HTTPAUTH`, `TS_RDB`,
`TS_DONTKILL`, `TS_PROXYURL`, `TS_PROXYMODE`, …) are in the
[root README](../README.md#running-in-docker--docker-compose).

The image is assembled from the repo-root `Dockerfile.runtime` by the
`docker` job in `.github/workflows/build.yml` (it reuses the binaries the build
matrix already cross-compiled — no recompile, no QEMU for the binary itself).

--------

Original upstream docker file and scripts by
[butaford (aka Pavel)](https://github.com/butaford).
