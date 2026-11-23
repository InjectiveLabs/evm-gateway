# syntax=docker/dockerfile:1.7
FROM golang:1.27.4-bookworm AS builder

ARG VERSION_FLAGS=""

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    make \
    gcc \
    libc-dev \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_CFLAGS="-O2 -g -D__BLST_PORTABLE__"
RUN go install -ldflags "${VERSION_FLAGS}" ./cmd/evm-gateway

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

ADD --chmod=0755 --checksum=sha256:c89f26f62458f3499102bef3a4ca1ddf3d1aff3b564208dc5aec84022d47842a \
    https://github.com/CosmWasm/wasmvm/releases/download/v2.2.10/libwasmvm.x86_64.so /lib/libwasmvm.x86_64.so
ADD --chmod=0755 --checksum=sha256:70125f597b4e408a87109d6316b81b7d9fc3645c9bf95772c89c2e7118873d3c \
    https://github.com/CosmWasm/wasmvm/releases/download/v2.2.10/libwasmvm.aarch64.so /lib/libwasmvm.aarch64.so

COPY --from=builder /go/bin/evm-gateway /usr/local/bin/evm-gateway

WORKDIR /apps/data

CMD ["evm-gateway"]
