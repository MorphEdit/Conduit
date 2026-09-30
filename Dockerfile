FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/conduit ./cmd/conduit

FROM alpine:3.20
RUN adduser -D -u 10001 conduit
COPY --from=build /out/conduit /usr/local/bin/conduit
USER conduit
EXPOSE 7420
ENTRYPOINT ["conduit"]
CMD ["-config", "/etc/conduit/conduit.yaml"]
