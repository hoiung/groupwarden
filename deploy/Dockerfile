# groupwarden in a container, for adopters who run containers rather than
# systemd (docs/deploy.md). Build from the repo root:
#   docker build -f deploy/Dockerfile -t groupwarden .
# Mount the config directory at /config (config.yaml, secrets file) and a
# volume for data_dir at /data; pair once with `docker run -it ... pair`.

FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go SQLite: no cgo, so the binary is static.
RUN CGO_ENABLED=0 go build -trimpath -o /out/groupwarden ./cmd/groupwarden && mkdir -m 0700 /out/data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/groupwarden /groupwarden
# /data must exist and belong to nonroot in the image: a new named volume
# copies it, owner included. Without it the volume is root's and the bot
# cannot write its database.
COPY --from=build --chown=nonroot:nonroot /out/data /data
USER nonroot:nonroot
ENV GROUPWARDEN_CONFIG=/config/config.yaml
# No config sync timer runs here (a changed config is reloaded with SIGHUP),
# so the bot does not alert that the sync is overdue.
ENV GROUPWARDEN_NO_SYNC_TIMER=1
VOLUME ["/data"]
# Healthy = connected, not deaf, config loaded, admin chat reached (the same
# checks as the systemd install's health check).
HEALTHCHECK --interval=1m --timeout=10s --start-period=3m CMD ["/groupwarden", "healthcheck"]
ENTRYPOINT ["/groupwarden"]
CMD ["run"]
