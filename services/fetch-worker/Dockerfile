# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/fetch-worker .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 fetch \
    && adduser -S -u 10001 -G fetch -H -s /sbin/nologin fetch

COPY --from=build /out/fetch-worker /usr/local/bin/fetch-worker

USER fetch
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/fetch-worker"]
