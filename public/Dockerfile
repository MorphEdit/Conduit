# Conduit container image, built from the published release binary.
#   docker build -t conduit .
#   docker build --build-arg VERSION=0.1.0 -t conduit:0.1.0 .
FROM alpine:3.20

ARG VERSION=0.1.0
ARG TARGETARCH=amd64

# pg_dump/psql copy table definitions to a newly joining site.
RUN apk add --no-cache ca-certificates postgresql16-client \
 && adduser -D -u 10001 conduit

ADD --chmod=755 https://github.com/MorphEdit/conduit/releases/download/v${VERSION}/conduit_linux_${TARGETARCH} /usr/local/bin/conduit

LABEL org.opencontainers.image.title="Conduit" \
      org.opencontainers.image.description="Multi-site Postgres sync that keeps working offline" \
      org.opencontainers.image.vendor="MorphEdit" \
      org.opencontainers.image.url="https://github.com/MorphEdit/conduit" \
      org.opencontainers.image.licenses="LicenseRef-Conduit-Freeware"

USER conduit
# 7420/tcp dashboard, 7420/udp LAN discovery, 7443/tcp other sites (TLS)
EXPOSE 7420 7420/udp 7443
ENTRYPOINT ["conduit"]
