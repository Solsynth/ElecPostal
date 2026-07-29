# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG GIT_COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags="-s -w" \
  -o /out/elecpostal ./cmd

FROM debian:bookworm-slim
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
  rm -rf /var/lib/apt/lists/* /usr/share/doc/* /usr/share/man/* /usr/share/locale/* && \
  apt-get clean

COPY --from=build /out/elecpostal /usr/local/bin/elecpostal

EXPOSE 8080 9090 25 465 587 110 995 143 993
ENTRYPOINT ["elecpostal"]
