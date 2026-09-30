# Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
# Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
# Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.1.0
ARG COMMIT=
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/conduit-sync/conduit/internal/buildinfo.Version=${VERSION} -X github.com/conduit-sync/conduit/internal/buildinfo.Commit=${COMMIT}" \
    -o /out/conduit ./cmd/conduit

FROM alpine:3.20
# pg_dump/psql copy table definitions to a newly joining site.
RUN apk add --no-cache postgresql16-client
RUN adduser -D -u 10001 conduit
COPY --from=build /out/conduit /usr/local/bin/conduit
USER conduit
EXPOSE 7420
ENTRYPOINT ["conduit"]
CMD ["-config", "/etc/conduit/conduit.yaml"]
