FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 works because modernc.org/sqlite is pure Go — no cgo, no
# libsqlite3 at runtime, so the final image can be a plain static binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/clubdir .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata wget \
	&& addgroup -S clubdir && adduser -S clubdir -G clubdir \
	&& mkdir -p /data && chown clubdir:clubdir /data

COPY --from=build /out/clubdir /usr/local/bin/clubdir

USER clubdir
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080

ENV CD_ADDR=0.0.0.0:8080 \
	CD_DATA=/data

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
	CMD wget -q -O- http://127.0.0.1:8080/ >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/clubdir"]
